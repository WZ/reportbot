package main

import (
	"testing"
)

func TestBuildTFIDFIndex_TopK(t *testing.T) {
	items := []historicalItem{
		{Description: "Fix database connection pool timeout", SectionID: "S0_0", SectionLabel: "Infrastructure"},
		{Description: "Add user authentication OAuth flow", SectionID: "S1_0", SectionLabel: "Auth Service"},
		{Description: "Database migration for new schema", SectionID: "S0_0", SectionLabel: "Infrastructure"},
	}
	idx := buildTFIDFIndex(items)

	results := idx.topK("database connection issue", 2)
	if len(results) == 0 {
		t.Fatalf("expected at least one result for database query")
	}
	// The top result should be the database connection pool item.
	if results[0].SectionID != "S0_0" {
		t.Fatalf("expected top result to be Infrastructure, got %s", results[0].SectionID)
	}
	if results[0].Description != "Fix database connection pool timeout" {
		t.Fatalf("expected most similar item first, got %s", results[0].Description)
	}
}

func TestTopKForBatch_Deduplication(t *testing.T) {
	items := []historicalItem{
		{Description: "Fix database timeout", SectionID: "S0_0"},
		{Description: "Add user login", SectionID: "S1_0"},
		{Description: "Improve database query performance", SectionID: "S0_0"},
	}
	idx := buildTFIDFIndex(items)

	// Both queries should match "database" items, but dedup should apply.
	results := idx.topKForBatch([]string{"database timeout fix", "database performance"}, 5)
	descSeen := make(map[string]int)
	for _, r := range results {
		descSeen[r.Description]++
	}
	for desc, count := range descSeen {
		if count > 1 {
			t.Fatalf("expected no duplicates, but %q appeared %d times", desc, count)
		}
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"Fix bug-123 in API", []string{"fix", "bug", "123", "in", "api"}},
		{"UPPERCASE MiXeD", []string{"uppercase", "mixed"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := tokenize(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestCosineSim_Orthogonal(t *testing.T) {
	a := sparseVec{0: 1.0, 1: 0.0}
	b := sparseVec{2: 1.0, 3: 0.0}
	sim := cosineSim(a, b)
	if sim != 0 {
		t.Fatalf("expected zero similarity for orthogonal vectors, got %f", sim)
	}
}

func TestCosineSim_Identical(t *testing.T) {
	a := sparseVec{0: 1.0, 1: 2.0}
	sim := cosineSim(a, a)
	if sim < 0.999 || sim > 1.001 {
		t.Fatalf("expected similarity ~1.0 for identical vectors, got %f", sim)
	}
}

func TestBuildTFIDFIndex_Empty(t *testing.T) {
	idx := buildTFIDFIndex(nil)
	results := idx.topK("anything", 5)
	if len(results) != 0 {
		t.Fatalf("expected no results from empty index")
	}
}
