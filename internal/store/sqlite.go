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
	defer rows.Close()

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

// Create inserts a new mindcache with a freshly generated id and returns the
// stored record, including the database-assigned timestamps.
func (r *MindcacheRepo) Create(ctx context.Context, brief string, sourceUrls []string) (*model.Mindcache, error) {
	if sourceUrls == nil {
		sourceUrls = []string{}
	}
	urlsJSON, err := json.Marshal(sourceUrls)
	if err != nil {
		return nil, fmt.Errorf("marshal sourceUrls: %w", err)
	}

	id := xid.New().String()
	_, err = r.db.ExecContext(ctx, `INSERT INTO Mindcache (id, brief, sourceUrls) VALUES (?, ?, ?)`, id, brief, string(urlsJSON))
	if err != nil {
		return nil, fmt.Errorf("insert mindcache: %w", err)
	}

	return r.GetByID(ctx, id)
}

// Update replaces the brief and sourceUrls of the mindcache with the given id
// and refreshes its updatedAt timestamp.
func (r *MindcacheRepo) Update(ctx context.Context, id string, brief string, sourceUrls []string) error {
	if sourceUrls == nil {
		sourceUrls = []string{}
	}
	urlsJSON, err := json.Marshal(sourceUrls)
	if err != nil {
		return fmt.Errorf("marshal sourceUrls: %w", err)
	}

	_, err = r.db.ExecContext(ctx, `UPDATE Mindcache SET brief = ?, sourceUrls = ?, updatedAt = CURRENT_TIMESTAMP WHERE id = ?`, brief, string(urlsJSON), id)
	if err != nil {
		return fmt.Errorf("update mindcache: %w", err)
	}

	return nil
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

type scanner interface {
	Scan(dest ...any) error
}

func scanMindcache(s scanner) (*model.Mindcache, error) {
	var (
		m          model.Mindcache
		sourceUrls string
		createdAt  time.Time
	)
	if err := s.Scan(&m.ID, &m.Brief, &sourceUrls, &createdAt, &m.UpdatedAt); err != nil {
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
