package slackbot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestParseGenerateReportArgs(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMode    string
		wantPrivate bool
		wantErr     bool
	}{
		{name: "default", input: "", wantMode: "team", wantPrivate: false},
		{name: "team private", input: "team private", wantMode: "team", wantPrivate: true},
		{name: "boss private", input: "boss private", wantMode: "boss", wantPrivate: true},
		{name: "private only", input: "private", wantMode: "team", wantPrivate: true},
		{name: "boss channel", input: "boss channel", wantMode: "boss", wantPrivate: false},
		{name: "unknown token", input: "boss now", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMode, gotPrivate, err := parseGenerateReportArgs(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected parseGenerateReportArgs to fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGenerateReportArgs returned error: %v", err)
			}
			if gotMode != tt.wantMode || gotPrivate != tt.wantPrivate {
				t.Fatalf("parseGenerateReportArgs(%q) = mode=%q private=%v, want mode=%q private=%v",
					tt.input, gotMode, gotPrivate, tt.wantMode, tt.wantPrivate)
			}
		})
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

func TestFormatItemDescriptionForList(t *testing.T) {
	tests := []struct {
		name string
		item WorkItem
		want string
	}{
		{
			name: "prepend single ticket id",
			item: WorkItem{
				Description: "Tune query timeout for widget pipeline",
				TicketIDs:   "7003001",
			},
			want: "[7003001] Tune query timeout for widget pipeline",
		},
		{
			name: "prepend comma list and trim spaces",
			item: WorkItem{
				Description: "Improve cache warm-up sequence",
				TicketIDs:   "7003002, 7003003",
			},
			want: "[7003002,7003003] Improve cache warm-up sequence",
		},
		{
			name: "no duplicate when description already has same prefix",
			item: WorkItem{
				Description: "[7003004] Add schema validation guard",
				TicketIDs:   "7003004",
			},
			want: "[7003004] Add schema validation guard",
		},
		{
			name: "no ticket ids leaves description unchanged",
			item: WorkItem{
				Description: "Cleanup stale deployment artifacts",
				TicketIDs:   "",
			},
			want: "Cleanup stale deployment artifacts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatItemDescriptionForList(tt.item)
			if got != tt.want {
				t.Fatalf("formatItemDescriptionForList() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveBossReportFromTeamReport_FileExists(t *testing.T) {
	dir := t.TempDir()
	teamName := "TestTeam"
	weekStart := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

	// Create a team report file
	teamReportContent := `### TestTeam 20260220

#### Engineering
- **Alice** - Implement feature X (in progress)
- **Bob** - Fix bug Y (done)
`
	teamReportFile := fmt.Sprintf("%s_%s.md", teamName, weekStart.Format("20060102"))
	teamReportPath := filepath.Join(dir, teamReportFile)
	if err := os.WriteFile(teamReportPath, []byte(teamReportContent), 0644); err != nil {
		t.Fatalf("failed to create team report: %v", err)
	}

	// Call the helper function
	filePath, bossReport, err := deriveBossReportFromTeamReport(dir, teamName, weekStart)
	if err != nil {
		t.Fatalf("deriveBossReportFromTeamReport returned error: %v", err)
	}
	if filePath == "" {
		t.Fatal("expected non-empty filePath")
	}
	if bossReport == "" {
		t.Fatal("expected non-empty bossReport")
	}

	// Verify the boss report was generated
	if !strings.Contains(bossReport, "Engineering") {
		t.Errorf("boss report should contain 'Engineering': %s", bossReport)
	}
	// Boss report should not have author prefixes in items
	if strings.Contains(bossReport, "**Alice**") {
		t.Errorf("boss report should not contain author prefixes: %s", bossReport)
	}

	// Verify the file was written
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("boss report file was not created: %v", err)
	}
}

func TestDeriveBossReportFromTeamReport_FileMissing(t *testing.T) {
	dir := t.TempDir()
	teamName := "TestTeam"
	weekStart := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

	// Do not create a team report file

	// Call the helper function
	filePath, bossReport, err := deriveBossReportFromTeamReport(dir, teamName, weekStart)
	if err != nil {
		t.Fatalf("deriveBossReportFromTeamReport returned error for missing file: %v", err)
	}
	if filePath != "" {
		t.Errorf("expected empty filePath when file missing, got %q", filePath)
	}
	if bossReport != "" {
		t.Errorf("expected empty bossReport when file missing, got %q", bossReport)
	}
}

func TestDeriveBossReportFromTeamReport_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	teamName := "TestTeam"
	weekStart := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

	// Create an empty team report file
	teamReportFile := fmt.Sprintf("%s_%s.md", teamName, weekStart.Format("20060102"))
	teamReportPath := filepath.Join(dir, teamReportFile)
	if err := os.WriteFile(teamReportPath, []byte(""), 0644); err != nil {
		t.Fatalf("failed to create empty team report: %v", err)
	}

	// Call the helper function
	filePath, bossReport, err := deriveBossReportFromTeamReport(dir, teamName, weekStart)
	if err != nil {
		t.Fatalf("deriveBossReportFromTeamReport returned error for empty file: %v", err)
	}
	if filePath != "" {
		t.Errorf("expected empty filePath when file empty, got %q", filePath)
	}
	if bossReport != "" {
		t.Errorf("expected empty bossReport when file empty, got %q", bossReport)
	}
}

func TestDeriveBossReportFromTeamReport_InvalidTeamName(t *testing.T) {
	dir := t.TempDir()
	weekStart := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

	// Test with path separators in team name
	invalidNames := []string{"Team/Name", "Team\\Name", "../Team"}
	for _, teamName := range invalidNames {
		filePath, bossReport, err := deriveBossReportFromTeamReport(dir, teamName, weekStart)
		if err == nil {
			t.Errorf("expected error for invalid team name %q, got none", teamName)
		}
		if filePath != "" {
			t.Errorf("expected empty filePath for invalid team name, got %q", filePath)
		}
		if bossReport != "" {
			t.Errorf("expected empty bossReport for invalid team name, got %q", bossReport)
		}
	}
}

func TestDeriveBossReportFromTeamReport_MalformedContent(t *testing.T) {
	dir := t.TempDir()
	teamName := "TestTeam"
	weekStart := time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

	// Create a team report with malformed content (but non-empty)
	teamReportContent := "This is not a valid markdown report\nJust some random text\n"
	teamReportFile := fmt.Sprintf("%s_%s.md", teamName, weekStart.Format("20060102"))
	teamReportPath := filepath.Join(dir, teamReportFile)
	if err := os.WriteFile(teamReportPath, []byte(teamReportContent), 0644); err != nil {
		t.Fatalf("failed to create malformed team report: %v", err)
	}

	// Call the helper function - it should still succeed (parseTemplate handles any input)
	filePath, bossReport, err := deriveBossReportFromTeamReport(dir, teamName, weekStart)
	if err != nil {
		t.Fatalf("deriveBossReportFromTeamReport returned error for malformed content: %v", err)
	}
	// Even with malformed content, we should get a file and report (parseTemplate is lenient)
	if filePath == "" {
		t.Error("expected non-empty filePath even with malformed content")
	}
	if bossReport == "" {
		t.Error("expected non-empty bossReport even with malformed content")
	}
}
