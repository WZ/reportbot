package main

import (
	"testing"
)

func TestFormatFetchSummary_AllFailed(t *testing.T) {
	result := FetchResult{
		TotalFetched: 0,
		Errors:       []string{"GitLab: connection refused"},
	}
	got := FormatFetchSummary(result)
	want := "Error fetching MRs/PRs:\nGitLab: connection refused"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_NoneToAdd(t *testing.T) {
	result := FetchResult{
		TotalFetched:   10,
		AlreadyTracked: 7,
		SkippedNonTeam: 3,
	}
	got := FormatFetchSummary(result)
	want := "Found 10 MRs/PRs (merged+open), none to add (7 already tracked, 3 non-team)."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_NoneToAddOnlyTracked(t *testing.T) {
	result := FetchResult{
		TotalFetched:   5,
		AlreadyTracked: 5,
	}
	got := FormatFetchSummary(result)
	want := "Found 5 MRs/PRs (merged+open), none to add (5 already tracked)."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_SomeInserted(t *testing.T) {
	result := FetchResult{
		TotalFetched:   15,
		Inserted:       8,
		AlreadyTracked: 5,
		SkippedNonTeam: 2,
	}
	got := FormatFetchSummary(result)
	want := "Fetched 15 MRs/PRs (merged+open): 8 new, 5 already tracked, 2 non-team"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_InsertedWithWarnings(t *testing.T) {
	result := FetchResult{
		TotalFetched: 10,
		Inserted:     3,
		Errors:       []string{"GitHub: rate limited"},
	}
	got := FormatFetchSummary(result)
	want := "Fetched 10 MRs/PRs (merged+open): 3 new\nWarnings:\nGitHub: rate limited"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatFetchSummary_ZeroFetchedNoErrors(t *testing.T) {
	result := FetchResult{
		TotalFetched: 0,
	}
	got := FormatFetchSummary(result)
	want := "Found 0 MRs/PRs (merged+open), none to add."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFetchAndImportMRs_NeitherConfigured(t *testing.T) {
	cfg := Config{}
	_, err := FetchAndImportMRs(cfg, nil)
	if err == nil {
		t.Fatal("expected error when neither source is configured")
	}
	if got := err.Error(); got != "neither GitLab nor GitHub is configured" {
		t.Errorf("unexpected error: %q", got)
	}
}
