package llm

import (
	"testing"

	"reportbot/internal/domain"
)

func TestSimilarIndexTopK(t *testing.T) {
	items := []domain.PlacedItem{
		{WorkItemID: 1, Description: "Fix database connection pool timeout", SectionID: 10},
		{WorkItemID: 2, Description: "Add user authentication OAuth flow", SectionID: 11},
		{WorkItemID: 3, Description: "Database migration for new schema", SectionID: 10},
	}
	idx := NewSimilarIndex(items)

	results := idx.TopK("database connection issue", 2, nil)
	if len(results) == 0 {
		t.Fatalf("expected at least one result for database query")
	}
	if results[0].WorkItemID != 1 {
		t.Fatalf("expected most similar item first, got %+v", results[0])
	}
}

func TestSimilarIndexTopKFilterAndPersonTieBreak(t *testing.T) {
	items := []domain.PlacedItem{
		{WorkItemID: 1, Description: "upgrade cluster nodes", SectionID: 10},
		{WorkItemID: 2, Description: "upgrade cluster nodes", SectionID: 11, ByPerson: true},
		{WorkItemID: 3, Description: "upgrade cluster nodes", SectionID: 12},
	}
	idx := NewSimilarIndex(items)
	results := idx.TopK("upgrade cluster", 3, func(p domain.PlacedItem) bool { return p.WorkItemID != 3 })
	if len(results) != 2 {
		t.Fatalf("expected filter to drop one item, got %d", len(results))
	}
	if results[0].WorkItemID != 2 {
		t.Fatalf("expected person-decided item first on a tie, got %+v", results[0])
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

func TestSimilarIndexEmpty(t *testing.T) {
	idx := NewSimilarIndex(nil)
	results := idx.TopK("anything", 5, nil)
	if len(results) != 0 {
		t.Fatalf("expected no results from empty index")
	}
}
