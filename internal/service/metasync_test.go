package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/xid"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
)

// syncMachine is one server instance in a multi-machine test: its own SQLite
// database, sharing a single file:// bucket directory with the other machines.
type syncMachine struct {
	repo    *store.MindcacheRepo
	search  *store.SearchRepo
	storage *Storage
	sync    *MetaSyncService
}

func newSyncMachine(t *testing.T, bucketDir string) *syncMachine {
	t.Helper()

	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "mindcache.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.RunMigrations(db); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	repo := store.NewMindcacheRepo(db)
	searchRepo := store.NewSearchRepo(db)
	storage, err := NewStorage(context.Background(), "file://"+bucketDir)
	if err != nil {
		t.Fatalf("NewStorage file://: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	return &syncMachine{
		repo:    repo,
		search:  searchRepo,
		storage: storage,
		sync:    NewMetaSyncService(repo, searchRepo, storage, nil),
	}
}

// createMindcache mirrors the production create flow: content first, then the
// database row, then the meta.json sidecar with its applied marker.
func createMindcache(t *testing.T, ctx context.Context, m *syncMachine, brief string, urls []string, content string) *model.Mindcache {
	t.Helper()

	id := xid.New().String()
	now := time.Now().UTC()
	if err := m.storage.Write(ctx, model.MindcacheMainPath(id), []byte(content)); err != nil {
		t.Fatalf("write main content: %v", err)
	}
	mc, err := m.repo.CreateWithID(ctx, id, brief, urls, now, now)
	if err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if err := WriteMeta(ctx, m.storage, mc); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	markMetaApplied(ctx, m.repo, m.storage, id)
	return mc
}

// touchSidecar forces the sidecar's modification time forward, simulating a
// later write regardless of filesystem timestamp granularity.
func touchSidecar(t *testing.T, bucketDir, id string, at time.Time) {
	t.Helper()
	p := filepath.Join(bucketDir, "mindcache", id, "meta.json")
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatalf("chtimes sidecar: %v", err)
	}
}

func TestMetaSyncAdoptsRemoteMindcache(t *testing.T) {
	ctx := context.Background()
	bucketDir := t.TempDir()
	machineA := newSyncMachine(t, bucketDir)
	machineB := newSyncMachine(t, bucketDir)

	created := createMindcache(t, ctx, machineA, "Docker networking", []string{"https://chatgpt.com/c/1"}, "bridge vs host modes")

	machineB.sync.Reconcile(ctx)

	got, err := machineB.repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID on machine B: %v", err)
	}
	if got == nil {
		t.Fatal("machine B did not adopt the remote mindcache")
	}
	if got.Brief != "Docker networking" || len(got.SourceURLs) != 1 || got.SourceURLs[0] != "https://chatgpt.com/c/1" {
		t.Fatalf("adopted = %+v, want matching brief and sourceUrls", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("adopted timestamps = %v / %v, want %v / %v", got.CreatedAt, got.UpdatedAt, created.CreatedAt, created.UpdatedAt)
	}

	// A second reconcile is a no-op: still exactly one row, same state.
	machineB.sync.Reconcile(ctx)
	all, err := machineB.repo.List(ctx)
	if err != nil {
		t.Fatalf("List on machine B: %v", err)
	}
	if len(all) != 1 || all[0].Brief != "Docker networking" {
		t.Fatalf("after second reconcile = %+v, want one stable row", all)
	}
}

func TestMetaSyncAppliesRemoteUpdate(t *testing.T) {
	ctx := context.Background()
	bucketDir := t.TempDir()
	machineA := newSyncMachine(t, bucketDir)
	machineB := newSyncMachine(t, bucketDir)

	created := createMindcache(t, ctx, machineA, "original brief", nil, "original content")
	machineB.sync.Reconcile(ctx)

	// Machine A integrates new content: content, row, then sidecar.
	updatedAt := time.Now().UTC().Add(time.Hour)
	if err := machineA.storage.Write(ctx, model.MindcacheMainPath(created.ID), []byte("updated content")); err != nil {
		t.Fatalf("rewrite content: %v", err)
	}
	if err := machineA.repo.UpdateWithTime(ctx, created.ID, "updated brief", []string{"https://chatgpt.com/c/2"}, updatedAt); err != nil {
		t.Fatalf("UpdateWithTime: %v", err)
	}
	updated, err := machineA.repo.GetByID(ctx, created.ID)
	if err != nil || updated == nil {
		t.Fatalf("GetByID after update: %v, %v", updated, err)
	}
	if err := WriteMeta(ctx, machineA.storage, updated); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	touchSidecar(t, bucketDir, created.ID, time.Now().Add(2*time.Hour))

	machineB.sync.Reconcile(ctx)

	got, err := machineB.repo.GetByID(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID on machine B: %v, %v", got, err)
	}
	if got.Brief != "updated brief" || len(got.SourceURLs) != 1 || got.SourceURLs[0] != "https://chatgpt.com/c/2" {
		t.Fatalf("refreshed = %+v, want updated brief and sourceUrls", got)
	}
	if !got.UpdatedAt.Equal(updatedAt.Truncate(time.Second)) && !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("refreshed updatedAt = %v, want %v", got.UpdatedAt, updatedAt)
	}
}

func TestMetaSyncPropagatesDelete(t *testing.T) {
	ctx := context.Background()
	bucketDir := t.TempDir()
	machineA := newSyncMachine(t, bucketDir)
	machineB := newSyncMachine(t, bucketDir)

	created := createMindcache(t, ctx, machineA, "to delete", nil, "content")
	machineB.sync.Reconcile(ctx)
	if got, _ := machineB.repo.GetByID(ctx, created.ID); got == nil {
		t.Fatal("machine B missed the initial adoption")
	}

	// Machine A deletes: the whole bucket prefix and the local row vanish.
	if err := machineA.repo.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := machineA.storage.DeleteDir(ctx, model.MindcachePrefix(created.ID)); err != nil {
		t.Fatalf("DeleteDir: %v", err)
	}

	machineB.sync.Reconcile(ctx)

	if got, err := machineB.repo.GetByID(ctx, created.ID); err != nil || got != nil {
		t.Fatalf("after remote delete: got = %+v, err = %v, want nil row", got, err)
	}
	states, err := machineB.repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("sync state = %v, want empty after remote delete", states)
	}
}

func TestMetaSyncSeedsLegacyContent(t *testing.T) {
	ctx := context.Background()
	bucketDir := t.TempDir()
	machineA := newSyncMachine(t, bucketDir)
	machineB := newSyncMachine(t, bucketDir)

	// Legacy layout: row and content exist, but no sidecar was ever written.
	mc, err := machineA.repo.Create(ctx, "legacy brief", []string{"https://gemini.google.com/app/1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := machineA.storage.Write(ctx, model.MindcacheMainPath(mc.ID), []byte("legacy content")); err != nil {
		t.Fatalf("write content: %v", err)
	}

	machineA.sync.Reconcile(ctx)

	data, err := machineA.storage.Read(ctx, model.MindcacheMetaPath(mc.ID))
	if err != nil {
		t.Fatalf("sidecar was not seeded: %v", err)
	}
	var meta model.MetaFile
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse seeded sidecar: %v", err)
	}
	if meta.Schema != model.MetaSchema || meta.Brief != "legacy brief" || len(meta.SourceURLs) != 1 {
		t.Fatalf("seeded sidecar = %+v", meta)
	}

	// The seed is marked applied locally, so the next pass does not reseed.
	states, err := machineA.repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if states[mc.ID].MetaModTime == "" {
		t.Fatal("metaModTime not recorded after seeding")
	}

	// And another machine adopts the seeded mindcache.
	machineB.sync.Reconcile(ctx)
	got, err := machineB.repo.GetByID(ctx, mc.ID)
	if err != nil || got == nil {
		t.Fatalf("machine B did not adopt seeded mindcache: %v, %v", got, err)
	}
	if got.Brief != "legacy brief" {
		t.Fatalf("adopted = %+v, want legacy brief", got)
	}
}

func TestMetaSyncEmptyBucketIsNoop(t *testing.T) {
	ctx := context.Background()
	machine := newSyncMachine(t, t.TempDir())
	machine.sync.Reconcile(ctx)

	states, err := machine.repo.ListSyncState(ctx)
	if err != nil {
		t.Fatalf("ListSyncState: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("sync state = %v, want empty", states)
	}
}

func TestWriteMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	storage, err := NewStorage(ctx, "mem://")
	if err != nil {
		t.Fatalf("NewStorage mem://: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })

	mc := &model.Mindcache{
		ID:         "abc123",
		Brief:      "brief",
		CreatedAt:  time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		SourceURLs: nil,
	}
	if err := WriteMeta(ctx, storage, mc); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	data, err := storage.Read(ctx, model.MindcacheMetaPath(mc.ID))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var meta model.MetaFile
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if meta.Schema != model.MetaSchema {
		t.Fatalf("schema = %d, want %d", meta.Schema, model.MetaSchema)
	}
	if meta.SourceURLs == nil || len(meta.SourceURLs) != 0 {
		t.Fatalf("sourceUrls = %v, want empty non-nil slice", meta.SourceURLs)
	}
	if !meta.CreatedAt.Equal(mc.CreatedAt) || !meta.UpdatedAt.Equal(mc.UpdatedAt) {
		t.Fatalf("timestamps = %v / %v, want %v / %v", meta.CreatedAt, meta.UpdatedAt, mc.CreatedAt, mc.UpdatedAt)
	}
}
