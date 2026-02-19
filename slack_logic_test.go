package main

import (
	"testing"
	"time"
)

func TestParseReportItemsSingleAndSharedStatus(t *testing.T) {
	loc := time.UTC

	single, err := parseReportItems("Fix login flow (in testing)", "Alice", loc)
	if err != nil {
		t.Fatalf("parseReportItems single failed: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("expected 1 item, got %d", len(single))
	}
	if single[0].Description != "Fix login flow" || single[0].Status != "in testing" {
		t.Fatalf("unexpected single item parse: %+v", single[0])
	}

	multi, err := parseReportItems("Item A\nItem B\n(in progress)", "Bob", loc)
	if err != nil {
		t.Fatalf("parseReportItems multiline failed: %v", err)
	}
	if len(multi) != 2 {
		t.Fatalf("expected 2 items, got %d", len(multi))
	}
	for _, it := range multi {
		if it.Status != "in progress" {
			t.Fatalf("expected shared status 'in progress', got %q", it.Status)
		}
	}
}

func TestParseReportItemsInvalidInput(t *testing.T) {
	if _, err := parseReportItems("   \n", "Alice", time.UTC); err == nil {
		t.Fatal("expected parseReportItems to fail for empty input")
	}
	if _, err := parseReportItems("(done)", "Alice", time.UTC); err == nil {
		t.Fatal("expected parseReportItems to fail when no description lines are present")
	}
}

func TestResolveDelegatedAuthorName(t *testing.T) {
	team := []string{"Alice Smith", "Bob Lee"}

	if got, ok := resolveDelegatedAuthorName("Alice Smith", team); !ok || got != "Alice Smith" {
		t.Fatalf("expected exact delegated name match, got %q", got)
	}

	// Fuzzy match should resolve to the only compatible team member.
	if got, ok := resolveDelegatedAuthorName("Alice", team); !ok || got != "Alice Smith" {
		t.Fatalf("expected fuzzy delegated name resolution, got %q", got)
	}

	// No match should be rejected.
	if got, ok := resolveDelegatedAuthorName("Charlie", team); ok || got != "" {
		t.Fatalf("expected unresolved delegated name to be rejected, got ok=%v value=%q", ok, got)
	}

	// Ambiguous match should be rejected.
	ambiguousTeam := []string{"Alice Smith", "Alice Wong"}
	if got, ok := resolveDelegatedAuthorName("Alice", ambiguousTeam); ok || got != "" {
		t.Fatalf("expected ambiguous delegated name to be rejected, got ok=%v value=%q", ok, got)
	}
}

func TestMapMRStatusAndReportedAt(t *testing.T) {
	base := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)

	opened := GitLabMR{
		State:     "opened",
		UpdatedAt: base.Add(1 * time.Hour),
		CreatedAt: base,
	}
	if got := mapMRStatus(opened); got != "in progress" {
		t.Fatalf("expected opened MR to map to in progress, got %q", got)
	}
	if got := mrReportedAt(opened, time.UTC); !got.Equal(opened.UpdatedAt) {
		t.Fatalf("expected opened MR to use UpdatedAt, got %v", got)
	}

	merged := GitLabMR{
		State:    "merged",
		MergedAt: base.Add(2 * time.Hour),
	}
	if got := mapMRStatus(merged); got != "done" {
		t.Fatalf("expected merged MR to map to done, got %q", got)
	}
	if got := mrReportedAt(merged, time.UTC); !got.Equal(merged.MergedAt) {
		t.Fatalf("expected merged MR to use MergedAt, got %v", got)
	}
}
