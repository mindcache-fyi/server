package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSearchRepo(t *testing.T) *SearchRepo {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return NewSearchRepo(db)
}

func TestUpsertSearchDocReplaces(t *testing.T) {
	repo := newTestSearchRepo(t)
	ctx := context.Background()

	if err := repo.UpsertSearchDoc(ctx, "id1", "2026-01-01T00:00:00Z", "brief one", "alpha beta gamma"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := repo.UpsertSearchDoc(ctx, "id1", "2026-01-02T00:00:00Z", "brief two", "delta epsilon"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	indexed, err := repo.ListIndexed(ctx)
	if err != nil {
		t.Fatalf("ListIndexed: %v", err)
	}
	if len(indexed) != 1 || indexed["id1"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("indexed = %v, want single id1 with fresh modTime", indexed)
	}
}

func TestSearchDocsMatchesSubstringAndRanks(t *testing.T) {
	repo := newTestSearchRepo(t)
	ctx := context.Background()

	docs := []struct{ id, brief, content string }{
		{"hit", "Database tuning", "Postgres vacuum strategies improve database performance a lot."},
		{"other", "Cooking pasta", "Boil water, add salt, cook the pasta."},
	}
	for _, d := range docs {
		if err := repo.UpsertSearchDoc(ctx, d.id, "2026-01-01T00:00:00Z", d.brief, d.content); err != nil {
			t.Fatalf("upsert %s: %v", d.id, err)
		}
	}

	hits, err := repo.SearchDocs(ctx, "vacuum strategies", 10)
	if err != nil {
		t.Fatalf("SearchDocs: %v", err)
	}
	if len(hits) != 1 || hits[0].MindcacheID != "hit" {
		t.Fatalf("hits = %+v, want only id=hit", hits)
	}
	if !strings.Contains(hits[0].Snippet, "\x01") || !strings.Contains(hits[0].Snippet, "\x02") {
		t.Fatalf("snippet missing highlight markers: %q", hits[0].Snippet)
	}
	if hits[0].Score >= 0 {
		t.Fatalf("bm25 score = %v, want negative", hits[0].Score)
	}

	// Phrase quoting: double quotes inside the query must not break syntax.
	if _, err := repo.SearchDocs(ctx, `he said "hi"`, 10); err != nil {
		t.Fatalf("SearchDocs with quotes: %v", err)
	}
}

func TestSearchSubstrCoversShortCJKQueries(t *testing.T) {
	repo := newTestSearchRepo(t)
	ctx := context.Background()

	if err := repo.UpsertSearchDoc(ctx, "cjk", "2026-01-01T00:00:00Z", "数据库性能",
		"数据库性能调优涉及查询优化、连接池配置和索引选择。"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	hits, err := repo.SearchSubstr(ctx, "连接池", 10)
	if err != nil {
		t.Fatalf("SearchSubstr: %v", err)
	}
	if len(hits) != 1 || hits[0].MindcacheID != "cjk" {
		t.Fatalf("hits = %+v, want cjk", hits)
	}
	if !strings.Contains(hits[0].Snippet, "\x01连接池\x02") {
		t.Fatalf("snippet = %q, want marked 连接池", hits[0].Snippet)
	}

	// LIKE wildcards in user input stay literal.
	more, err := repo.SearchSubstr(ctx, "%", 10)
	if err != nil {
		t.Fatalf("SearchSubstr %%: %v", err)
	}
	if len(more) != 0 {
		t.Fatalf("%% matched %d docs, want 0", len(more))
	}
}

func TestDeleteSearchDocIsIdempotent(t *testing.T) {
	repo := newTestSearchRepo(t)
	ctx := context.Background()

	if err := repo.DeleteSearchDoc(ctx, "ghost"); err != nil {
		t.Fatalf("delete unindexed: %v", err)
	}
	if err := repo.UpsertSearchDoc(ctx, "id1", "t", "b", "c"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repo.DeleteSearchDoc(ctx, "id1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	indexed, _ := repo.ListIndexed(ctx)
	if len(indexed) != 0 {
		t.Fatalf("indexed = %v, want empty", indexed)
	}
}

func TestBuildSnippetWindowAndMarkers(t *testing.T) {
	long := strings.Repeat("x", 200) + " needle " + strings.Repeat("y", 300)
	got := buildSnippet(long, "needle")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("snippet = %q, want leading and trailing ellipses", got)
	}
	if !strings.Contains(got, "\x01needle\x02") {
		t.Fatalf("snippet = %q, want marked needle", got)
	}

	short := "prefix needle suffix"
	if got := buildSnippet(short, "needle"); got != "prefix \x01needle\x02 suffix" {
		t.Fatalf("snippet = %q", got)
	}

	// No occurrence: head of the text.
	if got := buildSnippet("abcdefghij", "zzz"); got != "abcdefghij" {
		t.Fatalf("snippet = %q, want plain head", got)
	}

	// Case-insensitive matching.
	if got := buildSnippet("Find NEEDLE here", "needle"); !strings.Contains(got, "\x01NEEDLE\x02") {
		t.Fatalf("snippet = %q, want case-insensitive match", got)
	}
}
