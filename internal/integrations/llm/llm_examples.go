package llm

import (
	"math"
	"reportbot/internal/domain"
	"sort"
	"strings"
	"unicode"
)

type sparseVec = map[int]float64

// SimilarIndex finds the placed Work Items most similar to a text (TF-IDF cosine).
type SimilarIndex struct {
	vocab map[string]int
	idf   []float64
	docs  []sparseVec
	items []domain.PlacedItem
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func NewSimilarIndex(items []domain.PlacedItem) *SimilarIndex {
	if len(items) == 0 {
		return &SimilarIndex{vocab: make(map[string]int)}
	}

	// Build vocabulary.
	vocab := make(map[string]int)
	for _, item := range items {
		for _, tok := range tokenize(item.Description) {
			if _, ok := vocab[tok]; !ok {
				vocab[tok] = len(vocab)
			}
		}
	}

	// Document frequency.
	df := make([]int, len(vocab))
	docs := make([]sparseVec, len(items))
	n := float64(len(items))

	for i, item := range items {
		tokens := tokenize(item.Description)
		tf := make(map[int]int)
		for _, tok := range tokens {
			if idx, ok := vocab[tok]; ok {
				tf[idx]++
			}
		}
		vec := make(sparseVec, len(tf))
		for idx, count := range tf {
			vec[idx] = float64(count)
			df[idx]++
		}
		docs[i] = vec
	}

	// IDF.
	idf := make([]float64, len(vocab))
	for i, d := range df {
		if d > 0 {
			idf[i] = math.Log(n/float64(d)) + 1.0
		}
	}

	// Apply TF-IDF weighting.
	for _, vec := range docs {
		for idx := range vec {
			vec[idx] *= idf[idx]
		}
	}

	return &SimilarIndex{
		vocab: vocab,
		idf:   idf,
		docs:  docs,
		items: items,
	}
}

func (idx *SimilarIndex) queryVec(query string) sparseVec {
	tokens := tokenize(query)
	tf := make(map[int]int)
	for _, tok := range tokens {
		if i, ok := idx.vocab[tok]; ok {
			tf[i]++
		}
	}
	vec := make(sparseVec, len(tf))
	for i, count := range tf {
		vec[i] = float64(count) * idx.idf[i]
	}
	return vec
}

// TopK returns up to k items most similar to query, skipping items keep rejects.
// Ties go to person-decided items.
func (idx *SimilarIndex) TopK(query string, k int, keep func(domain.PlacedItem) bool) []domain.PlacedItem {
	if idx == nil || len(idx.items) == 0 || k <= 0 {
		return nil
	}
	qvec := idx.queryVec(query)
	if len(qvec) == 0 {
		return nil
	}
	type scored struct {
		index int
		score float64
	}
	var results []scored
	for i, dvec := range idx.docs {
		if keep != nil && !keep(idx.items[i]) {
			continue
		}
		if sim := cosineSim(qvec, dvec); sim > 0 {
			results = append(results, scored{i, sim})
		}
	}
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].score != results[b].score {
			return results[a].score > results[b].score
		}
		return idx.items[results[a].index].ByPerson && !idx.items[results[b].index].ByPerson
	})
	if len(results) > k {
		results = results[:k]
	}
	out := make([]domain.PlacedItem, len(results))
	for i, r := range results {
		out[i] = idx.items[r.index]
	}
	return out
}

func cosineSim(a, b sparseVec) float64 {
	var dot, normA, normB float64
	for i, va := range a {
		if vb, ok := b[i]; ok {
			dot += va * vb
		}
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
