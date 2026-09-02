package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gocloud.dev/blob"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
)

// MetaSyncService reconciles the local Mindcache metadata table against the
// meta.json sidecars stored in the blob bucket next to each mindcache's
// content. The bucket is the source of truth for metadata: several machines
// sharing one bucket each keep a local SQLite copy for fast, offline-capable
// reads, and last-write-wins is arbitrated by the sidecar object's
// modification time in the bucket rather than by machine clocks.
type MetaSyncService struct {
	repo     *store.MindcacheRepo
	search   *store.SearchRepo
	storage  *Storage
	embedder EmbeddingsProvider
}

// NewMetaSyncService wires a MetaSyncService. embedder may be nil.
func NewMetaSyncService(repo *store.MindcacheRepo, search *store.SearchRepo, storage *Storage, embedder EmbeddingsProvider) *MetaSyncService {
	return &MetaSyncService{repo: repo, search: search, storage: storage, embedder: embedder}
}

// RunLoop runs an initial reconcile and repeats every interval until ctx is
// cancelled. It is designed to run in a background goroutine and never fails
// the app.
func (s *MetaSyncService) RunLoop(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("meta sync loop panicked", "recover", r)
		}
	}()

	s.Reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Reconcile(ctx)
		}
	}
}

// Reconcile performs one sync pass: sidecars newer than the locally applied
// version are applied, content without a sidecar gets one seeded from the
// local row (which also migrates pre-sync buckets), and local rows whose
// bucket objects vanished are removed — that is how deletions propagate.
func (s *MetaSyncService) Reconcile(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("meta sync reconcile panicked", "recover", r)
		}
	}()

	started := time.Now()

	// Local state is snapshotted before the bucket listing: creates write
	// content before inserting the row, so any row in the snapshot already
	// has bucket objects by the time the listing starts, and a listing that
	// observes them can never misclassify the row as remotely deleted.
	local, err := s.repo.ListSyncState(ctx)
	if err != nil {
		slog.Warn("meta sync: list local state failed", "error", err)
		return
	}

	objects, ok := s.listBucket(ctx)
	if !ok {
		return
	}

	ids := make([]string, 0, len(objects))
	for id := range objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var adopted, refreshed, seeded, removed int
	for _, id := range ids {
		obj := objects[id]
		state, exists := local[id]
		// Bucket objects exist, so this id is never a deletion candidate,
		// even when applying its sidecar fails transiently.
		delete(local, id)

		switch {
		case obj.hasMeta:
			if exists && !sidecarNewer(obj.metaModTime, state.MetaModTime) {
				continue
			}
			if err := s.applyMeta(ctx, id, obj.metaModTime); err != nil {
				slog.Warn("meta sync: apply sidecar failed", "mindcache", id, "error", err)
				continue
			}
			if exists {
				refreshed++
			} else {
				adopted++
			}
		case obj.hasMain && exists:
			if err := s.seedMeta(ctx, id); err != nil {
				slog.Warn("meta sync: seed sidecar failed", "mindcache", id, "error", err)
				continue
			}
			seeded++
		}
	}

	// Remaining local rows have no bucket objects at all: another machine
	// deleted the mindcache.
	for id := range local {
		if err := s.repo.Delete(ctx, id); err != nil {
			slog.Warn("meta sync: delete stale row failed", "mindcache", id, "error", err)
			continue
		}
		if err := s.search.DeleteSearchDoc(ctx, id); err != nil {
			slog.Warn("meta sync: delete search doc failed", "mindcache", id, "error", err)
		}
		removed++
	}

	if adopted+refreshed+seeded+removed > 0 {
		slog.Info("meta sync reconcile complete",
			"adopted", adopted, "refreshed", refreshed, "seeded", seeded, "removed", removed,
			"duration_ms", time.Since(started).Milliseconds())
	}
}

// bucketObject summarises the objects one bucket listing revealed about a
// single mindcache.
type bucketObject struct {
	hasMain     bool
	hasMeta     bool
	metaModTime time.Time
}

// listBucket groups every object under the mindcache root by mindcache id.
// It returns ok=false when the listing failed; callers must abort the pass so
// a transient error never triggers the deletion path.
func (s *MetaSyncService) listBucket(ctx context.Context) (map[string]*bucketObject, bool) {
	objects := make(map[string]*bucketObject)
	iter := s.storage.bucket.List(&blob.ListOptions{Prefix: model.MindcacheRoot})
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Warn("meta sync: list bucket failed", "error", err)
			return nil, false
		}
		rest := strings.TrimPrefix(obj.Key, model.MindcacheRoot)
		id, _, ok := strings.Cut(rest, "/")
		if !ok || id == "" {
			continue
		}
		entry := objects[id]
		if entry == nil {
			entry = &bucketObject{}
			objects[id] = entry
		}
		switch {
		case strings.HasSuffix(obj.Key, model.MainSuffix):
			entry.hasMain = true
		case strings.HasSuffix(obj.Key, model.MetaSuffix):
			entry.hasMeta = true
			entry.metaModTime = obj.ModTime.UTC()
		}
	}
	return objects, true
}

// sidecarNewer reports whether the bucket sidecar (modTime) is newer than the
// one this machine last wrote or applied. An empty or unparseable applied
// marker always re-applies, which converges within one extra download.
func sidecarNewer(modTime time.Time, applied string) bool {
	if applied == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, applied)
	if err != nil {
		return true
	}
	return modTime.After(t)
}

// applyMeta downloads the sidecar of id and applies it to the local database,
// recording the sidecar's bucket modification time as the applied marker.
func (s *MetaSyncService) applyMeta(ctx context.Context, id string, modTime time.Time) error {
	data, err := s.storage.Read(ctx, model.MindcacheMetaPath(id))
	if err != nil {
		return fmt.Errorf("read sidecar: %w", err)
	}
	var meta model.MetaFile
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse sidecar: %w", err)
	}
	if meta.Schema != model.MetaSchema {
		return fmt.Errorf("unsupported sidecar schema %d", meta.Schema)
	}

	prev, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load local row: %w", err)
	}

	if err := s.repo.UpsertMeta(ctx, id, meta.Brief, meta.SourceURLs, meta.CreatedAt, meta.UpdatedAt, modTime.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("upsert: %w", err)
	}

	if prev == nil || prev.Brief != meta.Brief {
		if err := s.search.UpdateBrief(ctx, id, meta.Brief); err != nil {
			slog.Warn("meta sync: update search brief failed", "mindcache", id, "error", err)
		}
		embedBrief(ctx, s.embedder, s.repo, id, meta.Brief)
	}
	return nil
}

// seedMeta writes the sidecar of a mindcache that has bucket content but no
// sidecar yet, sourcing the values from the local row.
func (s *MetaSyncService) seedMeta(ctx context.Context, id string) error {
	mc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("load local row: %w", err)
	}
	if mc == nil {
		return nil
	}
	if err := WriteMeta(ctx, s.storage, mc); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	markMetaApplied(ctx, s.repo, s.storage, id)
	return nil
}

// WriteMeta stores the metadata sidecar of a mindcache in the blob bucket.
func WriteMeta(ctx context.Context, storage *Storage, mc *model.Mindcache) error {
	meta := model.MetaFile{
		Schema:     model.MetaSchema,
		Brief:      mc.Brief,
		SourceURLs: mc.SourceURLs,
		CreatedAt:  mc.CreatedAt,
		UpdatedAt:  mc.UpdatedAt,
	}
	if meta.SourceURLs == nil {
		meta.SourceURLs = []string{}
	}
	data, err := json.Marshal(&meta)
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	return storage.Write(ctx, model.MindcacheMetaPath(mc.ID), data)
}

// markMetaApplied records the sidecar's current bucket modification time on
// the local row so the next reconcile does not re-apply this machine's own
// write. It falls back to the local clock when attributes are unavailable.
func markMetaApplied(ctx context.Context, repo *store.MindcacheRepo, storage *Storage, id string) {
	modTime := time.Now().UTC()
	if mt, err := storage.ModTime(ctx, model.MindcacheMetaPath(id)); err == nil {
		modTime = mt.UTC()
	}
	if err := repo.SetMetaModTime(ctx, id, modTime.Format(time.RFC3339Nano)); err != nil {
		slog.Warn("meta sync: mark sidecar applied failed", "mindcache", id, "error", err)
	}
}
