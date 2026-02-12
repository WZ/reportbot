package main

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

type historicalItem struct {
	Description  string
	SectionID    string
	SectionLabel string
}

type sparseVec = map[int]float64

type tfidfIndex struct {
	vocab map[string]int
	idf   []float64
	docs  []sparseVec
	items []historicalItem
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

func buildTFIDFIndex(items []historicalItem) *tfidfIndex {
	if len(items) == 0 {
		return &tfidfIndex{vocab: make(map[string]int)}
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

	return &tfidfIndex{
		vocab: vocab,
		idf:   idf,
		docs:  docs,
		items: items,
	}
}

func (idx *tfidfIndex) queryVec(query string) sparseVec {
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

// topKIndices returns the indices of the top-K most similar items to query.
func (idx *tfidfIndex) topKIndices(query string, k int) []int {
	if len(idx.items) == 0 || k <= 0 {
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
		sim := cosineSim(qvec, dvec)
		if sim > 0 {
			results = append(results, scored{i, sim})
		}
	}
	sort.Slice(results, func(a, b int) bool {
		return results[a].score > results[b].score
	})
	if len(results) > k {
		results = results[:k]
	}
	out := make([]int, len(results))
	for i, r := range results {
		out[i] = r.index
	}
	return out
}

func (idx *tfidfIndex) topK(query string, k int) []historicalItem {
	indices := idx.topKIndices(query, k)
	out := make([]historicalItem, len(indices))
	for i, docIdx := range indices {
		out[i] = idx.items[docIdx]
	}
	return out
}

func (idx *tfidfIndex) topKForBatch(queries []string, k int) []historicalItem {
	if len(idx.items) == 0 || k <= 0 {
		return nil
	}
	seen := make(map[int]bool)
	var out []historicalItem
	for _, q := range queries {
		for _, docIdx := range idx.topKIndices(q, k) {
			if !seen[docIdx] {
				seen[docIdx] = true
				out = append(out, idx.items[docIdx])
			}
		}
	}
	if len(out) > k {
		out = out[:k]
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
