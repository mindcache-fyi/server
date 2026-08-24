package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
)

// newTestIndexer builds a SearchIndexService over a temp SQLite database and
// an in-memory blob bucket, mirroring the production wiring.
func newTestIndexer(t *testing.T) (*SearchIndexService, *store.MindcacheRepo, *Storage) {
	t.Helper()

	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	repo := store.NewMindcacheRepo(db)
	searchRepo := store.NewSearchRepo(db)
	storage, err := NewStorage(context.Background(), "mem://")
	if err != nil {
		t.Fatalf("NewStorage mem://: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	return NewSearchIndexService(repo, searchRepo, storage), repo, storage
}

func TestReconcileIndexesAndCleansUp(t *testing.T) {
	svc, repo, storage := newTestIndexer(t)
	ctx := context.Background()

	mc1, err := repo.Create(ctx, "Docker networking", nil)
	if err != nil {
		t.Fatalf("create mc1: %v", err)
	}
	mc2, err := repo.Create(ctx, "Pasta recipes", nil)
	if err != nil {
		t.Fatalf("create mc2: %v", err)
	}

	for _, d := range []struct{ id, content string }{
		{mc1.ID, "## Networking\n\nBridge mode isolates containers from the host."},
		{mc2.ID, "Boil water, add salt."},
	} {
		if err := storage.Write(ctx, model.MindcacheMainPath(d.id), []byte(d.content)); err != nil {
			t.Fatalf("seed %s: %v", d.id, err)
		}
	}

	svc.Reconcile(ctx)

	res, err := svc.Search(ctx, "bridge mode isolates", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].Mindcache.ID != mc1.ID {
		t.Fatalf("results = %+v, want only %s", resultsIDs(res), mc1.ID)
	}
	if !containsString(res[0].MatchedIn, "content") {
		t.Fatalf("matchedIn = %v, want content", res[0].MatchedIn)
	}
	if res[0].Mindcache.Brief != "Docker networking" {
		t.Fatalf("brief = %q, want joined metadata", res[0].Mindcache.Brief)
	}

	// Deleting the blob and reconciling removes the index entry.
	if err := storage.DeleteDir(ctx, model.MindcachePrefix(mc2.ID)); err != nil {
		t.Fatalf("delete dir: %v", err)
	}
	svc.Reconcile(ctx)

	res, err = svc.Search(ctx, "boil water", 10)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("results = %+v, want empty after reconcile", resultsIDs(res))
	}
}

func TestReconcileRefreshesModifiedContent(t *testing.T) {
	svc, repo, storage := newTestIndexer(t)
	ctx := context.Background()

	mc, err := repo.Create(ctx, "Kubernetes", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := storage.Write(ctx, model.MindcacheMainPath(mc.ID), []byte("pods and services")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc.Reconcile(ctx)

	// Out-of-band edit: newer mtime must be picked up on the next reconcile.
	time.Sleep(1100 * time.Millisecond) // file-style mtimes have 1s granularity
	if err := storage.Write(ctx, model.MindcacheMainPath(mc.ID), []byte("deployments and daemonsets")); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	svc.Reconcile(ctx)

	res, err := svc.Search(ctx, "daemonsets", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("results = %+v, want refreshed doc to match", resultsIDs(res))
	}
}

func TestIndexMindcacheWriteThroughPath(t *testing.T) {
	svc, repo, storage := newTestIndexer(t)
	ctx := context.Background()

	mc, err := repo.Create(ctx, "Redis caching patterns", []string{"https://chatgpt.com/c/1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := storage.Write(ctx, model.MindcacheMainPath(mc.ID), []byte("Use ttl based eviction for cache invalidation.")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.IndexMindcache(ctx, mc.ID); err != nil {
		t.Fatalf("IndexMindcache: %v", err)
	}

	// Brief match (short query exercises the LIKE fallback path); the word
	// does not appear in the content.
	res, err := svc.Search(ctx, "redis", 10)
	if err != nil {
		t.Fatalf("search brief: %v", err)
	}
	if len(res) != 1 || !containsString(res[0].MatchedIn, "brief") || containsString(res[0].MatchedIn, "content") {
		t.Fatalf("results = %+v, want brief-only match", res)
	}

	// Two-character CJK query also routes through the fallback path.
	if err := repo.Update(ctx, mc.ID, "缓存淘汰策略", mc.SourceURLs); err != nil {
		t.Fatalf("update brief: %v", err)
	}
	if err := svc.IndexMindcache(ctx, mc.ID); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	res, err = svc.Search(ctx, "缓存", 10)
	if err != nil {
		t.Fatalf("search cjk: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("results = %+v, want 2-char CJK match via fallback", resultsIDs(res))
	}
}

func TestSearchEmptyQueryReturnsNoResults(t *testing.T) {
	svc, _, _ := newTestIndexer(t)
	res, err := svc.Search(context.Background(), "   ", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("results = %+v, want empty", res)
	}
}

func resultsIDs(results []model.SearchResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.Mindcache.ID
	}
	return ids
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
