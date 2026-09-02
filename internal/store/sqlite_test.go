package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestRepo(t *testing.T) *MindcacheRepo {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return NewMindcacheRepo(db)
}

func TestCreateWithIDStoresExplicitTimestamps(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	updatedAt := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	mc, err := repo.CreateWithID(ctx, "fixed-id", "brief one", []string{"https://x.test/1"}, createdAt, updatedAt)
	if err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if mc.ID != "fixed-id" || mc.Brief != "brief one" {
		t.Fatalf("mc = %+v, want fixed-id / brief one", mc)
	}
	if !mc.CreatedAt.Equal(createdAt) || !mc.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("timestamps = %v / %v, want %v / %v", mc.CreatedAt, mc.UpdatedAt, createdAt, updatedAt)
	}
	if len(mc.SourceURLs) != 1 || mc.SourceURLs[0] != "https://x.test/1" {
		t.Fatalf("sourceUrls = %v", mc.SourceURLs)
	}
}

func TestUpsertMetaInsertsThenOverwrites(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	createdAt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)

	// Insert path: no local row yet.
	if err := repo.UpsertMeta(ctx, "remote-1", "remote brief", []string{"https://r.test/1"}, createdAt, updatedAt, "2026-01-03T00:00:00Z"); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	mc, err := repo.GetByID(ctx, "remote-1")
	if err != nil || mc == nil {
		t.Fatalf("GetByID after upsert: %v, %v", mc, err)
	}
	if mc.Brief != "remote brief" || !mc.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("mc = %+v, want remote brief / %v", mc, updatedAt)
	}

	// Overwrite path: newer remote snapshot wins.
	newer := updatedAt.Add(24 * time.Hour)
	if err := repo.UpsertMeta(ctx, "remote-1", "updated brief", nil, createdAt, newer, "2026-01-04T00:00:00Z"); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	mc, err = repo.GetByID(ctx, "remote-1")
	if err != nil || mc == nil {
		t.Fatalf("GetByID after second upsert: %v, %v", mc, err)
	}
	if mc.Brief != "updated brief" || !mc.UpdatedAt.Equal(newer) {
		t.Fatalf("mc = %+v, want updated brief / %v", mc, newer)
	}
	if len(mc.SourceURLs) != 0 {
		t.Fatalf("sourceUrls = %v, want empty after nil overwrite", mc.SourceURLs)
	}

	states, err := repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if states["remote-1"].MetaModTime != "2026-01-04T00:00:00Z" {
		t.Fatalf("metaModTime = %q, want latest applied marker", states["remote-1"].MetaModTime)
	}
}

func TestUpsertMetaPreservesEmbedding(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if _, err := repo.CreateWithID(ctx, "with-emb", "brief", nil, now, now); err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if err := repo.SetEmbedding(ctx, "with-emb", []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("SetEmbedding: %v", err)
	}

	if err := repo.UpsertMeta(ctx, "with-emb", "new brief", nil, now, now.Add(time.Hour), "m1"); err != nil {
		t.Fatalf("UpsertMeta: %v", err)
	}

	embs, err := repo.ListEmbeddings(ctx)
	if err != nil {
		t.Fatalf("ListEmbeddings: %v", err)
	}
	if got := embs["with-emb"]; len(got) != 4 || got[0] != 1 {
		t.Fatalf("embedding = %v, want preserved", got)
	}
}

func TestListSyncStateHandlesNullMarker(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if _, err := repo.Create(ctx, "local only", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	states, err := repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %v, want one row", states)
	}
	for _, st := range states {
		if st.MetaModTime != "" {
			t.Fatalf("metaModTime = %q, want empty for never-synced row", st.MetaModTime)
		}
		if st.UpdatedAt.IsZero() {
			t.Fatal("updatedAt is zero, want set")
		}
	}
}

func TestSetMetaModTime(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	mc, err := repo.Create(ctx, "brief", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.SetMetaModTime(ctx, mc.ID, "2026-02-01T00:00:00Z"); err != nil {
		t.Fatalf("SetMetaModTime: %v", err)
	}
	states, err := repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if states[mc.ID].MetaModTime != "2026-02-01T00:00:00Z" {
		t.Fatalf("metaModTime = %q", states[mc.ID].MetaModTime)
	}
}

func TestUpdateWithTimeSetsExplicitTimestamp(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	mc, err := repo.Create(ctx, "brief", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	later := mc.UpdatedAt.Add(48 * time.Hour).Truncate(time.Second)
	if err := repo.UpdateWithTime(ctx, mc.ID, "new brief", []string{"https://u.test"}, later); err != nil {
		t.Fatalf("UpdateWithTime: %v", err)
	}
	got, err := repo.GetByID(ctx, mc.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID: %v, %v", got, err)
	}
	if got.Brief != "new brief" || !got.UpdatedAt.Equal(later) {
		t.Fatalf("got = %+v, want new brief / %v", got, later)
	}
}
