package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
)

// modTimeEqual compares two timestamps at one-second granularity; local
// filesystem mtimes (fileblob) lack sub-second precision.
func modTimeEqual(a time.Time, b string) bool {
	stored, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		return false
	}
	return a.Unix() == stored.Unix()
}

// SearchIndexService keeps the FTS index in sync with the blob content and
// answers full-text queries. The index is derived data: Reconcile can rebuild
// it from scratch, and per-write hooks keep it fresh between reconciles.
// Content is always read through the blob abstraction, so file:// and s3://
// backends share the same pipeline — remote protocols benefit from the
// ModTime-based incremental sync, which avoids re-downloading unchanged
// documents.
type SearchIndexService struct {
	repo    *store.MindcacheRepo
	search  *store.SearchRepo
	storage *Storage
}

// NewSearchIndexService wires the indexer over the given dependencies.
func NewSearchIndexService(repo *store.MindcacheRepo, search *store.SearchRepo, storage *Storage) *SearchIndexService {
	return &SearchIndexService{repo: repo, search: search, storage: storage}
}

// IndexMindcache reads the mindcache's main content and (re)indexes it,
// recording the object's modification time. Missing content indexes an
// empty document so briefs alone stay searchable.
func (s *SearchIndexService) IndexMindcache(ctx context.Context, id string) error {
	key := model.MindcacheMainPath(id)
	content := ""
	data, err := s.storage.Read(ctx, key)
	switch {
	case err == nil:
		content = string(data)
	case gcerrors.Code(err) == gcerrors.NotFound:
		// No content yet: index the brief alone.
	default:
		// A transient read failure must not overwrite the entry with
		// empty content stamped with a fresh modTime — reconcile would
		// then consider it up to date. Surface the error instead; the
		// caller logs it and the next reconcile repairs the drift.
		return fmt.Errorf("read main content: %w", err)
	}

	modTime := time.Now().UTC()
	if mt, err := s.storage.ModTime(ctx, key); err == nil {
		modTime = mt.UTC()
	}

	brief := ""
	if mc, err := s.repo.GetByID(ctx, id); err == nil && mc != nil {
		brief = mc.Brief
	}

	return s.search.UpsertSearchDoc(ctx, id, modTime.Format(time.RFC3339Nano), brief, ExtractPlainText(content))
}

// RemoveFromIndex drops the mindcache's indexed document.
func (s *SearchIndexService) RemoveFromIndex(ctx context.Context, id string) {
	if err := s.search.DeleteSearchDoc(ctx, id); err != nil {
		slog.Warn("remove search doc failed", "mindcache", id, "error", err)
	}
}

// Reconcile synchronises the index with the bucket: new or modified
// main.md objects are re-indexed, and index entries whose object vanished
// are removed. It is designed to run in a background goroutine and never
// fails the app.
func (s *SearchIndexService) Reconcile(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("search reconcile panicked", "recover", r)
		}
	}()

	started := time.Now()
	briefs, err := s.search.ListBriefs(ctx)
	if err != nil {
		slog.Warn("search reconcile: list briefs failed", "error", err)
		return
	}
	indexed, err := s.search.ListIndexed(ctx)
	if err != nil {
		slog.Warn("search reconcile: list indexed docs failed", "error", err)
		return
	}

	const suffix = "/main.md"
	iter := s.storage.bucket.List(&blob.ListOptions{Prefix: model.MindcacheRoot})
	refreshed := 0
	for {
		obj, err := iter.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Warn("search reconcile: list bucket failed", "error", err)
			return
		}
		if !strings.HasSuffix(obj.Key, suffix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(obj.Key, model.MindcacheRoot), suffix)
		if have, ok := indexed[id]; ok && modTimeEqual(obj.ModTime.UTC(), have) {
			delete(indexed, id)
			continue
		}
		if err := s.indexDoc(ctx, id, obj.ModTime.UTC(), briefs[id]); err != nil {
			slog.Warn("search reconcile: index doc failed", "mindcache", id, "error", err)
			continue
		}
		refreshed++
		delete(indexed, id)
	}

	removed := 0
	for id := range indexed {
		if err := s.search.DeleteSearchDoc(ctx, id); err != nil {
			slog.Warn("search reconcile: remove stale doc failed", "mindcache", id, "error", err)
			continue
		}
		removed++
	}

	if refreshed > 0 || removed > 0 {
		slog.Info("search reconcile complete", "indexed", refreshed, "removed", removed, "duration_ms", time.Since(started).Milliseconds())
	}
}

// indexDoc indexes one document with an already-known modification time,
// skipping the extra attributes round-trip.
func (s *SearchIndexService) indexDoc(ctx context.Context, id string, modTime time.Time, brief string) error {
	data, err := s.storage.Read(ctx, model.MindcacheMainPath(id))
	if err != nil {
		return fmt.Errorf("read main content: %w", err)
	}
	return s.search.UpsertSearchDoc(ctx, id, modTime.Format(time.RFC3339Nano), brief, ExtractPlainText(string(data)))
}

// Search runs a full-text query over briefs and content. Queries of three or
// more characters use the trigram index with bm25 ranking; shorter queries
// fall back to a LIKE scan because the trigram tokenizer cannot match them.
func (s *SearchIndexService) Search(ctx context.Context, query string, limit int) ([]model.SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []model.SearchResult{}, nil
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}

	var (
		hits []store.SearchHit
		err  error
	)
	if len([]rune(query)) >= TrigramMinRunes {
		hits, err = s.search.SearchDocs(ctx, query, limit)
	} else {
		hits, err = s.search.SearchSubstr(ctx, query, limit)
	}
	if err != nil {
		return nil, err
	}

	metas, err := s.search.GetByIDs(ctx, hitIDs(hits))
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.Mindcache, len(metas))
	for _, m := range metas {
		byID[m.ID] = m
	}

	results := make([]model.SearchResult, 0, len(hits))
	for _, h := range hits {
		mc, ok := byID[h.MindcacheID]
		if !ok {
			continue // metadata row vanished; next reconcile cleans the index
		}
		results = append(results, model.SearchResult{
			Mindcache: mc,
			MatchedIn: matchedIn(query, mc.Brief, h.Content),
			Snippet:   h.Snippet,
			Score:     -h.Score, // bm25 is negative-better; expose positive rank
		})
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results, nil
}

// Search limits shared with the HTTP handler.
const (
	// TrigramMinRunes is the shortest query the trigram index can match;
	// shorter queries fall back to a LIKE scan.
	TrigramMinRunes = 3
	// DefaultSearchLimit applies when the caller does not specify one.
	DefaultSearchLimit = 20
	// MaxSearchLimit caps the per-request result count.
	MaxSearchLimit = 100
)

func hitIDs(hits []store.SearchHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.MindcacheID
	}
	return ids
}

// matchedIn reports which indexed fields contain the query.
func matchedIn(query, brief, content string) []string {
	q := strings.ToLower(query)
	var out []string
	if strings.Contains(strings.ToLower(brief), q) {
		out = append(out, "brief")
	}
	if strings.Contains(strings.ToLower(content), q) {
		out = append(out, "content")
	}
	return out
}
