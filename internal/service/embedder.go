package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mindcache-fyi/server/internal/model"
	"github.com/mindcache-fyi/server/internal/store"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/compat"
)

// EmbeddingsProvider generates embeddings for short texts. It abstracts the
// embedding endpoint so the analyse pipeline can be tested with fakes and so
// the feature can be absent (nil) without conditionals everywhere.
type EmbeddingsProvider interface {
	// Embed returns one vector per input text, in order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dims reports the vector dimensionality established by the startup
	// probe, or 0 when unknown.
	Dims() int
}

// Embedder is an EmbeddingsProvider backed by an OpenAI-compatible
// /embeddings endpoint via the goai SDK.
type Embedder struct {
	model provider.EmbeddingModel
	sem   *Semaphore
	dims  int
}

// compile-time check
var _ EmbeddingsProvider = (*Embedder)(nil)

// NewEmbedder creates an Embedder and probes the endpoint once. It returns
// nil (feature disabled) when model or baseURL is empty, or when the probe
// fails — embeddings are an optional enhancement and must never break start.
// The return type is the interface so the disabled state is a true nil. A
// non-nil signer adds HMAC gateway headers; nil keeps plain BYOK behaviour.
func NewEmbedder(baseURL, apiKey, model string, maxConcurrency int, signer *RequestSigner) EmbeddingsProvider {
	if baseURL == "" || model == "" {
		return nil
	}

	opts := []compat.Option{
		compat.WithBaseURL(baseURL),
		compat.WithAPIKey(apiKey),
	}
	if signer != nil {
		opts = append(opts, compat.WithHTTPClient(&http.Client{
			Transport: signer.Transport(nil),
		}))
	}

	m := compat.Embedding(model, opts...)

	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := m.DoEmbed(probeCtx, []string{"mindcache probe"}, provider.EmbedParams{})
	if err != nil || len(res.Embeddings) != 1 || len(res.Embeddings[0]) == 0 {
		slog.Warn("embedding endpoint probe failed; embedding-based matching disabled",
			"baseURL", baseURL, "model", model, "error", err)
		return nil
	}

	slog.Info("embedding endpoint ready", "baseURL", baseURL, "model", model,
		"dims", len(res.Embeddings[0]))

	return &Embedder{
		model: m,
		sem:   NewSemaphore(maxConcurrency),
		dims:  len(res.Embeddings[0]),
	}
}

// Dims implements EmbeddingsProvider.
func (e *Embedder) Dims() int {
	return e.dims
}

// Embed implements EmbeddingsProvider. Vectors are converted to float32 and
// validated against the dimensionality established by the probe. Inputs are
// chunked when the provider limits batch size.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := e.sem.Acquire(ctx); err != nil {
		return nil, err
	}
	defer e.sem.Release()

	chunk := e.model.MaxValuesPerCall()
	if chunk <= 0 {
		chunk = len(texts)
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += chunk {
		end := min(start+chunk, len(texts))
		res, err := e.model.DoEmbed(ctx, texts[start:end], provider.EmbedParams{})
		if err != nil {
			return nil, fmt.Errorf("embed: %w", err)
		}
		if len(res.Embeddings) != end-start {
			return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(res.Embeddings), end-start)
		}
		for _, v := range res.Embeddings {
			if len(v) != e.dims {
				return nil, fmt.Errorf("embed: unexpected dimensionality %d, want %d", len(v), e.dims)
			}
			out = append(out, float64sToFloat32s(v))
		}
	}
	return out, nil
}

func float64sToFloat32s(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}

// embedBrief generates and persists the embedding of a mindcache brief. It
// is best-effort: failures are logged and never block knowledge saving.
func embedBrief(ctx context.Context, embedder EmbeddingsProvider, repo embeddingStore, id, brief string) {
	if embedder == nil || brief == "" {
		return
	}
	vecs, err := embedder.Embed(ctx, []string{brief})
	if err != nil || len(vecs) != 1 {
		slog.Warn("embed brief failed", "mindcache", id, "error", err)
		return
	}
	if err := repo.SetEmbedding(ctx, id, encodeFloat32s(vecs[0])); err != nil {
		slog.Warn("persist embedding failed", "mindcache", id, "error", err)
	}
}

// embeddingStore is the persistence surface the embedding feature needs.
// *store.MindcacheRepo satisfies it.
type embeddingStore interface {
	SetEmbedding(ctx context.Context, id string, blob []byte) error
	ListEmbeddings(ctx context.Context) (map[string][]byte, error)
}

// BackfillEmbeddings embeds every mindcache that has no usable embedding
// yet (missing or dimensionality mismatch). It is designed to run in a
// background goroutine and never fails the app.
func BackfillEmbeddings(ctx context.Context, repo *store.MindcacheRepo, embedder EmbeddingsProvider) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("embedding backfill panicked", "recover", r)
		}
	}()

	all, err := repo.List(ctx)
	if err != nil {
		slog.Warn("embedding backfill: list mindcaches failed", "error", err)
		return
	}
	stored, err := repo.ListEmbeddings(ctx)
	if err != nil {
		slog.Warn("embedding backfill: list embeddings failed", "error", err)
		return
	}

	pending := make([]model.Mindcache, 0)
	for _, mc := range all {
		blob, ok := stored[mc.ID]
		if !ok {
			pending = append(pending, mc)
			continue
		}
		if vec, err := decodeFloat32s(blob); err != nil || len(vec) != embedder.Dims() {
			pending = append(pending, mc)
		}
	}
	if len(pending) == 0 {
		return
	}

	slog.Info("embedding backfill starting", "count", len(pending))
	const batchSize = 32
	done := 0
	for start := 0; start < len(pending); start += batchSize {
		end := min(start+batchSize, len(pending))
		batch := pending[start:end]
		briefs := make([]string, len(batch))
		for i, mc := range batch {
			briefs[i] = mc.Brief
		}
		vecs, err := embedder.Embed(ctx, briefs)
		if err != nil || len(vecs) != len(batch) {
			slog.Warn("embedding backfill: batch failed", "error", err, "batch", len(batch))
			continue
		}
		for i, mc := range batch {
			if err := repo.SetEmbedding(ctx, mc.ID, encodeFloat32s(vecs[i])); err != nil {
				slog.Warn("embedding backfill: persist failed", "mindcache", mc.ID, "error", err)
				continue
			}
			done++
		}
	}
	slog.Info("embedding backfill finished", "embedded", done, "total", len(pending))
}
