// Package store provides SQLite-backed persistence for mindcache metadata,
// with schema migrations embedded directly into the binary so a deployment
// needs only a single executable and a data directory.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/mindcache-fyi/server/internal/model"
	"github.com/rs/xid"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenSQLite opens the SQLite database at dbPath, creating the parent
// directory when it does not exist, and applies the pragmas required for safe
// access under concurrent HTTP load: WAL journaling for concurrent reads, a
// 5s busy timeout instead of an immediate lock error, and NORMAL synchronous
// for a safe-but-fast WAL flush. The connection pool is pinned to a single
// connection because SQLite serializes writes; extra connections only add
// lock contention.
func OpenSQLite(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	return db, nil
}

// RunMigrations applies every embedded schema migration to db. It is
// idempotent: an already-current schema is treated as success rather than an
// error, so it is safe to call on every startup.
func RunMigrations(db *sql.DB) error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}

	driver, err := sqlite.WithInstance(db, &sqlite.Config{})
	if err != nil {
		return fmt.Errorf("migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "sqlite", driver)
	if err != nil {
		return fmt.Errorf("migration init: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migration up: %w", err)
	}

	return nil
}

// MindcacheRepo is a SQLite-backed repository for mindcache metadata.
type MindcacheRepo struct {
	db *sql.DB
}

// NewMindcacheRepo returns a MindcacheRepo that persists through db.
func NewMindcacheRepo(db *sql.DB) *MindcacheRepo {
	return &MindcacheRepo{db: db}
}

// List returns every mindcache ordered by most recently updated first.
func (r *MindcacheRepo) List(ctx context.Context) ([]model.Mindcache, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, brief, sourceUrls, createdAt, updatedAt FROM Mindcache ORDER BY updatedAt DESC`)
	if err != nil {
		return nil, fmt.Errorf("query mindcaches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	mindcaches := make([]model.Mindcache, 0)
	for rows.Next() {
		m, err := scanMindcache(rows)
		if err != nil {
			return nil, err
		}
		mindcaches = append(mindcaches, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mindcaches: %w", err)
	}

	return mindcaches, nil
}

// GetByID returns the mindcache with the given id. It returns (nil, nil) when
// no such mindcache exists so callers can distinguish "not found" from a real
// error without comparing against sentinel values.
func (r *MindcacheRepo) GetByID(ctx context.Context, id string) (*model.Mindcache, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id, brief, sourceUrls, createdAt, updatedAt FROM Mindcache WHERE id = ?`, id)

	m, err := scanMindcache(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return m, nil
}

// sqlTime renders a timestamp in the UTC layout modernc.org/sqlite parses
// back into time.Time, matching SQLite's own CURRENT_TIMESTAMP precision.
func sqlTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.999999999")
}

// Create inserts a new mindcache with a freshly generated id and returns the
// stored record, including the assigned timestamps. It delegates to
// CreateWithID using the current time for both timestamps.
func (r *MindcacheRepo) Create(ctx context.Context, brief string, sourceUrls []string) (*model.Mindcache, error) {
	now := time.Now().UTC()
	return r.CreateWithID(ctx, xid.New().String(), brief, sourceUrls, now, now)
}

// CreateWithID inserts a new mindcache with a caller-supplied id and
// timestamps and returns the stored record. Callers that also write a
// meta.json sidecar pass the same timestamps so the row and the sidecar
// agree exactly.
func (r *MindcacheRepo) CreateWithID(ctx context.Context, id string, brief string, sourceUrls []string, createdAt, updatedAt time.Time) (*model.Mindcache, error) {
	if sourceUrls == nil {
		sourceUrls = []string{}
	}
	urlsJSON, err := json.Marshal(sourceUrls)
	if err != nil {
		return nil, fmt.Errorf("marshal sourceUrls: %w", err)
	}

	_, err = r.db.ExecContext(ctx,
		`INSERT INTO Mindcache (id, brief, sourceUrls, createdAt, updatedAt) VALUES (?, ?, ?, ?, ?)`,
		id, brief, string(urlsJSON), sqlTime(createdAt), sqlTime(updatedAt))
	if err != nil {
		return nil, fmt.Errorf("insert mindcache: %w", err)
	}

	return r.GetByID(ctx, id)
}

// Update replaces the brief and sourceUrls of the mindcache with the given id
// and refreshes its updatedAt timestamp.
func (r *MindcacheRepo) Update(ctx context.Context, id string, brief string, sourceUrls []string) error {
	return r.UpdateWithTime(ctx, id, brief, sourceUrls, time.Now().UTC())
}

// UpdateWithTime is Update with an explicit updatedAt, used by callers that
// must keep the local row and the meta.json sidecar timestamps identical.
func (r *MindcacheRepo) UpdateWithTime(ctx context.Context, id string, brief string, sourceUrls []string, updatedAt time.Time) error {
	if sourceUrls == nil {
		sourceUrls = []string{}
	}
	urlsJSON, err := json.Marshal(sourceUrls)
	if err != nil {
		return fmt.Errorf("marshal sourceUrls: %w", err)
	}

	_, err = r.db.ExecContext(ctx, `UPDATE Mindcache SET brief = ?, sourceUrls = ?, updatedAt = ? WHERE id = ?`, brief, string(urlsJSON), sqlTime(updatedAt), id)
	if err != nil {
		return fmt.Errorf("update mindcache: %w", err)
	}

	return nil
}

// UpsertMeta applies a full metadata snapshot synced from another machine's
// meta.json sidecar. Existing rows are overwritten; the embedding column is
// left untouched because embeddings are rederived locally. New rows are
// inserted so a mindcache created on one machine materialises on every other.
func (r *MindcacheRepo) UpsertMeta(ctx context.Context, id string, brief string, sourceUrls []string, createdAt, updatedAt time.Time, metaModTime string) error {
	if sourceUrls == nil {
		sourceUrls = []string{}
	}
	urlsJSON, err := json.Marshal(sourceUrls)
	if err != nil {
		return fmt.Errorf("marshal sourceUrls: %w", err)
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO Mindcache (id, brief, sourceUrls, createdAt, updatedAt, metaModTime)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			brief = excluded.brief,
			sourceUrls = excluded.sourceUrls,
			createdAt = excluded.createdAt,
			updatedAt = excluded.updatedAt,
			metaModTime = excluded.metaModTime`,
		id, brief, string(urlsJSON), sqlTime(createdAt), sqlTime(updatedAt), metaModTime)
	if err != nil {
		return fmt.Errorf("upsert meta: %w", err)
	}

	return nil
}

// SetMetaModTime records the blob modification time of the meta.json sidecar
// this machine last wrote, so the reconciler treats its own write as applied.
func (r *MindcacheRepo) SetMetaModTime(ctx context.Context, id string, metaModTime string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE Mindcache SET metaModTime = ? WHERE id = ?`, metaModTime, id)
	if err != nil {
		return fmt.Errorf("set metaModTime: %w", err)
	}
	return nil
}

// SyncState is the per-mindcache bookkeeping the metadata reconciler needs to
// compare the local database against the blob bucket.
type SyncState struct {
	UpdatedAt   time.Time
	MetaModTime string
}

// ListSyncState returns the sync bookkeeping of every mindcache keyed by id.
func (r *MindcacheRepo) ListSyncState(ctx context.Context) (map[string]SyncState, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, updatedAt, metaModTime FROM Mindcache`)
	if err != nil {
		return nil, fmt.Errorf("query sync state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]SyncState)
	for rows.Next() {
		var (
			id          string
			updatedAt   time.Time
			metaModTime sql.NullString
		)
		if err := rows.Scan(&id, &updatedAt, &metaModTime); err != nil {
			return nil, fmt.Errorf("scan sync state: %w", err)
		}
		out[id] = SyncState{UpdatedAt: updatedAt, MetaModTime: metaModTime.String}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync state: %w", err)
	}
	return out, nil
}

// Delete removes the mindcache with the given id. Deleting a non-existent id
// is not an error.
func (r *MindcacheRepo) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM Mindcache WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete mindcache: %w", err)
	}
	return nil
}

// SetEmbedding stores the embedding vector blob of the mindcache with the
// given id.
func (r *MindcacheRepo) SetEmbedding(ctx context.Context, id string, blob []byte) error {
	_, err := r.db.ExecContext(ctx, `UPDATE Mindcache SET embedding = ? WHERE id = ?`, blob, id)
	if err != nil {
		return fmt.Errorf("set embedding: %w", err)
	}
	return nil
}

// ListEmbeddings returns the raw embedding blobs of every mindcache that has
// one, keyed by mindcache id.
func (r *MindcacheRepo) ListEmbeddings(ctx context.Context) (map[string][]byte, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, embedding FROM Mindcache WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("query embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string][]byte)
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("scan embedding: %w", err)
		}
		out[id] = blob
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings: %w", err)
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMindcache(s scanner) (*model.Mindcache, error) {
	var (
		m          model.Mindcache
		sourceUrls string
	)
	if err := s.Scan(&m.ID, &m.Brief, &sourceUrls, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan mindcache: %w", err)
	}

	urls, err := parseSourceUrls(sourceUrls)
	if err != nil {
		return nil, err
	}
	m.SourceURLs = urls

	return &m, nil
}

func parseSourceUrls(raw string) ([]string, error) {
	if raw == "" {
		return []string{}, nil
	}
	var urls []string
	if err := json.Unmarshal([]byte(raw), &urls); err != nil {
		return nil, fmt.Errorf("parse sourceUrls: %w", err)
	}
	if urls == nil {
		urls = []string{}
	}
	return urls, nil
}
