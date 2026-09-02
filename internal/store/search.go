package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/mindcache-fyi/server/internal/model"
)

// SearchRepo manages the MindcacheFTS full-text index. The index is derived
// data — it can be rebuilt at any time from the blob content — so methods
// favour simplicity over transactional coupling with the metadata tables.
type SearchRepo struct {
	db *sql.DB
}

// NewSearchRepo returns a SearchRepo persisting through db.
func NewSearchRepo(db *sql.DB) *SearchRepo {
	return &SearchRepo{db: db}
}

// SearchHit is one ranked row from the FTS index.
type SearchHit struct {
	MindcacheID string
	Brief       string
	Content     string
	Snippet     string
	Score       float64
}

// UpsertSearchDoc replaces the indexed document of a mindcache. Delete and
// insert run in one transaction so a crash in between cannot lose the entry.
func (r *SearchRepo) UpsertSearchDoc(ctx context.Context, id, modTime, brief, content string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM MindcacheFTS WHERE mindcacheId = ?`, id); err != nil {
		return fmt.Errorf("delete search doc: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO MindcacheFTS (mindcacheId, modTime, brief, content) VALUES (?, ?, ?, ?)`,
		id, modTime, brief, content,
	); err != nil {
		return fmt.Errorf("insert search doc: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit upsert: %w", err)
	}
	return nil
}

// DeleteSearchDoc removes the indexed document of a mindcache. Deleting an
// unindexed id is not an error.
func (r *SearchRepo) DeleteSearchDoc(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM MindcacheFTS WHERE mindcacheId = ?`, id); err != nil {
		return fmt.Errorf("delete search doc: %w", err)
	}
	return nil
}

// UpdateBrief refreshes only the brief of an indexed mindcache. It is used by
// the metadata reconciler when a remote meta.json changes the brief but the
// main content (and thus the indexed document's modTime) is unchanged. When no
// document is indexed yet the update is a no-op; the next content reconcile
// inserts it with the current brief.
func (r *SearchRepo) UpdateBrief(ctx context.Context, id string, brief string) error {
	if _, err := r.db.ExecContext(ctx, `UPDATE MindcacheFTS SET brief = ? WHERE mindcacheId = ?`, brief, id); err != nil {
		return fmt.Errorf("update brief: %w", err)
	}
	return nil
}

// ListIndexed returns every indexed mindcache id with its stored modTime.
func (r *SearchRepo) ListIndexed(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT mindcacheId, modTime FROM MindcacheFTS`)
	if err != nil {
		return nil, fmt.Errorf("query indexed docs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var id, modTime string
		if err := rows.Scan(&id, &modTime); err != nil {
			return nil, fmt.Errorf("scan indexed doc: %w", err)
		}
		out[id] = modTime
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexed docs: %w", err)
	}
	return out, nil
}

// SearchDocs runs a full-text query against briefs and content using the
// trigram tokenizer, returning up to limit hits ranked by bm25. The query is
// treated as one literal substring phrase. Snippets carry \x01/\x02 markers
// around matches and "…" ellipses; the snippet column (-1) is chosen by
// SQLite from whichever column matched.
func (r *SearchRepo) SearchDocs(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	phrase := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	rows, err := r.db.QueryContext(ctx, `
		SELECT mindcacheId, brief, content,
		       snippet(MindcacheFTS, -1, char(1), char(2), '…', 16),
		       bm25(MindcacheFTS) AS score
		FROM MindcacheFTS
		WHERE MindcacheFTS MATCH ?
		ORDER BY score
		LIMIT ?`,
		phrase, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search docs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]SearchHit, 0)
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.MindcacheID, &h.Brief, &h.Content, &h.Snippet, &h.Score); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search hits: %w", err)
	}
	return hits, nil
}

// SearchSubstr is the fallback path for queries shorter than what the
// trigram tokenizer can index-match (fewer than three characters — common
// CJK words). It scans the indexed text with LIKE and builds snippets in Go;
// correctness matters more than speed at this scale.
func (r *SearchRepo) SearchSubstr(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	pattern := "%" + escapeLike(query) + "%"
	rows, err := r.db.QueryContext(ctx, `
		SELECT mindcacheId, brief, content
		FROM MindcacheFTS
		WHERE brief LIKE ? ESCAPE '\' OR content LIKE ? ESCAPE '\'
		ORDER BY mindcacheId`,
		pattern, pattern,
	)
	if err != nil {
		return nil, fmt.Errorf("search substr: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]SearchHit, 0)
	for rows.Next() {
		var h SearchHit
		var content string
		if err := rows.Scan(&h.MindcacheID, &h.Brief, &content); err != nil {
			return nil, fmt.Errorf("scan substr hit: %w", err)
		}
		h.Content = content
		h.Snippet = buildSnippet(content, query)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate substr hits: %w", err)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// ListBriefs returns the briefs of every mindcache, keyed by id. Used by the
// indexer when reconciling bucket content that carries no metadata.
func (r *SearchRepo) ListBriefs(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, brief FROM Mindcache`)
	if err != nil {
		return nil, fmt.Errorf("query briefs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var id, brief string
		if err := rows.Scan(&id, &brief); err != nil {
			return nil, fmt.Errorf("scan brief: %w", err)
		}
		out[id] = brief
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate briefs: %w", err)
	}
	return out, nil
}

// GetByIDs returns the metadata of the given mindcaches. Unknown ids are
// skipped so callers can join index hits against possibly-stale rows.
func (r *SearchRepo) GetByIDs(ctx context.Context, ids []string) ([]model.Mindcache, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, brief, sourceUrls, createdAt, updatedAt FROM Mindcache WHERE id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("query mindcaches: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.Mindcache, 0, len(ids))
	for rows.Next() {
		m, err := scanMindcache(rows)
		if err != nil {
			return nil, fmt.Errorf("scan mindcache: %w", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate mindcaches: %w", err)
	}
	return out, nil
}

// escapeLike escapes SQL LIKE wildcards in user input; pair with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

// buildSnippet carves a window around the first case-insensitive occurrence
// of query inside text, wrapping the match in \x01/\x02 markers like the
// FTS5 snippet() function does.
func buildSnippet(text, query string) string {
	const (
		window   = 160 // total rune budget of the returned snippet
		ctxRunes = 48  // context runes shown before the match
	)

	runes := []rune(text)
	lower := strings.ToLower(text)
	qLower := strings.ToLower(query)
	qLen := len([]rune(query))

	idx := strings.Index(lower, qLower)
	if idx < 0 {
		return truncateRunes(runes, window)
	}
	matchAt := len([]rune(lower[:idx]))

	from := max(matchAt-ctxRunes, 0)
	to := min(from+window, len(runes))

	var b strings.Builder
	if from > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(runes[from:matchAt]))
	b.WriteString("\x01")
	b.WriteString(string(runes[matchAt : matchAt+qLen]))
	b.WriteString("\x02")
	if tailEnd := min(matchAt+qLen+max(to-(matchAt+qLen), 0), len(runes)); tailEnd > matchAt+qLen {
		b.WriteString(string(runes[matchAt+qLen : tailEnd]))
	}
	if to < len(runes) {
		b.WriteString("…")
	}
	return b.String()
}

func truncateRunes(runes []rune, n int) string {
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + "…"
}
