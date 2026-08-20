package service

import (
	"math"
	"testing"
)

func almostEqual(a, b float32) bool {
	return math.Abs(float64(a-b)) < 1e-6
}

func TestCosineSimilarity(t *testing.T) {
	if got := cosineSimilarity([]float32{1, 0}, []float32{1, 0}); !almostEqual(got, 1) {
		t.Errorf("identical vectors = %v, want 1", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Errorf("orthogonal vectors = %v, want 0", got)
	}
	if got := cosineSimilarity([]float32{1, 2, 3}, []float32{1, 2}); got != 0 {
		t.Errorf("dimension mismatch = %v, want 0", got)
	}
	if got := cosineSimilarity([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Errorf("zero vector = %v, want 0", got)
	}
	if got := cosineSimilarity(nil, nil); got != 0 {
		t.Errorf("nil vectors = %v, want 0", got)
	}
}

func TestTopKIndices(t *testing.T) {
	vecs := [][]float32{
		{1, 0},   // identical to query
		{0, 1},   // orthogonal
		nil,      // missing
		{0.9, 0.1}, // close to query
	}
	got := topKIndices([]float32{1, 0}, vecs, 5)
	if len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Errorf("topK = %v, want [0 3] ordered by similarity", got)
	}

	got = topKIndices([]float32{1, 0}, vecs, 1)
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("topK with k=1 = %v, want [0]", got)
	}

	if got := topKIndices([]float32{1, 0}, vecs, 0); got != nil {
		t.Errorf("topK with k=0 = %v, want nil", got)
	}
}

func TestFloat32sRoundTrip(t *testing.T) {
	in := []float32{0, 1.5, -2.25, 1e-3}
	out, err := decodeFloat32s(encodeFloat32s(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], in[i])
		}
	}

	if _, err := decodeFloat32s([]byte{1, 2, 3}); err == nil {
		t.Error("expected error for blob length not a multiple of 4")
	}

	if got, err := decodeFloat32s(nil); err != nil || len(got) != 0 {
		t.Errorf("empty blob = %v, %v; want empty, nil", got, err)
	}
}
