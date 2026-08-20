package service

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// encodeFloat32s serializes a vector as little-endian float32 bytes.
func encodeFloat32s(vec []float32) []byte {
	blob := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(v))
	}
	return blob
}

// decodeFloat32s parses a little-endian float32 blob produced by
// encodeFloat32s.
func decodeFloat32s(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d not a multiple of 4", len(blob))
	}
	vec := make([]float32, len(blob)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec, nil
}

// cosineSimilarity returns the cosine similarity of two vectors. Vectors of
// different lengths, or zero-norm vectors, return 0.
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / math.Sqrt(normA*normB))
}

// topKIndices returns the indices of the k vectors most similar to vec,
// ordered by descending similarity. Zero-similarity entries are skipped.
func topKIndices(vec []float32, vecs [][]float32, k int) []int {
	if k <= 0 {
		return nil
	}
	type scored struct {
		idx   int
		score float32
	}
	scores := make([]scored, 0, len(vecs))
	for i, v := range vecs {
		s := cosineSimilarity(vec, v)
		if s <= 0 {
			continue
		}
		scores = append(scores, scored{idx: i, score: s})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		return scores[i].idx < scores[j].idx
	})
	if len(scores) > k {
		scores = scores[:k]
	}
	out := make([]int, len(scores))
	for i, s := range scores {
		out[i] = s.idx
	}
	return out
}
