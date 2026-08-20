package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zendev-sh/goai/provider"
)

// fakeEmbeddingModel is a scripted provider.EmbeddingModel for unit tests.
type fakeEmbeddingModel struct {
	dims        int
	maxPerCall  int
	batches     [][]string
	nextVectors func(values []string) [][]float64
	err         error
}

func (m *fakeEmbeddingModel) ModelID() string { return "fake" }

func (m *fakeEmbeddingModel) MaxValuesPerCall() int { return m.maxPerCall }

func (m *fakeEmbeddingModel) DoEmbed(_ context.Context, values []string, _ provider.EmbedParams) (*provider.EmbedResult, error) {
	m.batches = append(m.batches, values)
	if m.err != nil {
		return nil, m.err
	}
	return &provider.EmbedResult{Embeddings: m.nextVectors(values)}, nil
}

func vec64(vals ...float64) []float64 { return vals }

func TestEmbedder_ChunksBatchesAndConverts(t *testing.T) {
	fake := &fakeEmbeddingModel{
		dims:       2,
		maxPerCall: 2,
		nextVectors: func(values []string) [][]float64 {
			out := make([][]float64, len(values))
			for i := range values {
				out[i] = []float64{1, 0}
			}
			return out
		},
	}
	e := &Embedder{model: fake, sem: NewSemaphore(1), dims: 2}

	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("len(vecs) = %d, want 3", len(vecs))
	}
	if vecs[0][0] != 1 || vecs[0][1] != 0 {
		t.Errorf("vecs[0] = %v, want [1 0]", vecs[0])
	}
	if len(fake.batches) != 2 || len(fake.batches[0]) != 2 || len(fake.batches[1]) != 1 {
		t.Errorf("batches = %v, want [[a b] [c]]", fake.batches)
	}
}

func TestEmbedder_RejectsDimensionMismatch(t *testing.T) {
	fake := &fakeEmbeddingModel{
		maxPerCall: 0,
		nextVectors: func(values []string) [][]float64 {
			return [][]float64{{1, 0, 0}}
		},
	}
	e := &Embedder{model: fake, sem: NewSemaphore(1), dims: 2}

	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected dimensionality error")
	}
}

func TestEmbedder_UpstreamError(t *testing.T) {
	fake := &fakeEmbeddingModel{err: fmt.Errorf("boom")}
	e := &Embedder{model: fake, sem: NewSemaphore(1), dims: 2}

	if _, err := e.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected upstream error")
	}
}

// mockEmbeddingsServer speaks the OpenAI-compatible /embeddings wire format
// and returns scripted vectors in call order.
type mockEmbeddingsServer struct {
	mu      sync.Mutex
	vectors [][][]float64 // per call: one vector per input
	calls   int
}

func (m *mockEmbeddingsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/embeddings" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Input []string `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	m.mu.Lock()
	i := m.calls
	m.calls++
	var vectors [][]float64
	if i < len(m.vectors) {
		vectors = m.vectors[i]
	}
	m.mu.Unlock()

	data := make([]map[string]any, 0, len(req.Input))
	for idx := range req.Input {
		vec := []float64{}
		if idx < len(vectors) {
			vec = vectors[idx]
		}
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     idx,
			"embedding": vec,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
		"model":  "test-embed",
		"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
	})
}

func TestNewEmbedder_ProbeSuccess(t *testing.T) {
	// Call order: startup probe, then the Embed call below.
	mock := &mockEmbeddingsServer{vectors: [][][]float64{{{1, 0}}, {{1, 0}}}}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	e := NewEmbedder(srv.URL+"/v1", "key", "test-embed", 1)
	if e == nil {
		t.Fatal("expected enabled embedder after successful probe")
	}
	if e.Dims() != 2 {
		t.Errorf("Dims = %d, want 2", e.Dims())
	}

	vecs, err := e.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 || vecs[0][0] != 1 || vecs[0][1] != 0 {
		t.Errorf("vecs = %v, want [[1 0]]", vecs)
	}
}

func TestNewEmbedder_ProbeFailureDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if e := NewEmbedder(srv.URL+"/v1", "key", "test-embed", 1); e != nil {
		t.Error("expected nil embedder after failed probe")
	}
}

func TestNewEmbedder_UnconfiguredDisables(t *testing.T) {
	if e := NewEmbedder("", "key", "", 1); e != nil {
		t.Error("expected nil embedder without model/baseURL")
	}
}
