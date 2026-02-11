package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildReportsFromLast_FirstEverFallback(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		ReportOutputDir: dir,
		TeamName:        "TEAMX",
	}

	items := []WorkItem{
		{ID: 1, Author: "Taylor Stone", Description: "Implement X", Status: "in progress"},
		{ID: 2, Author: "Jordan Kim", Description: "Fix Y", Status: "done"},
	}

	result, err := BuildReportsFromLast(cfg, items, mustDate(t, "20260209"), nil)
	if err != nil {
		t.Fatalf("BuildReportsFromLast failed: %v", err)
	}
	merged := result.Template

	team := renderTeamMarkdown(merged)

	boss := renderBossMarkdown(merged)

	if !strings.Contains(team, "#### Undetermined") {
		t.Fatalf("team report should include Undetermined section:\n%s", team)
	}
	if !strings.Contains(team, "**Jordan Kim** - Fix Y (done)") {
		t.Fatalf("team report should include author per item:\n%s", team)
	}
	if !strings.Contains(boss, "Undetermined (") || !strings.Contains(boss, "Taylor Stone") || !strings.Contains(boss, "Jordan Kim") {
		t.Fatalf("boss report should include authors at category heading:\n%s", boss)
	}
	if strings.Contains(boss, "**Jordan Kim**") {
		t.Fatalf("boss report should not include author prefixes in items:\n%s", boss)
	}
}

func TestBuildReportsFromLast_MergeSortAndDoneRemoval(t *testing.T) {
	dir := t.TempDir()
	prev := `### TEAMX 20260202

#### Top Focus

- **Feature A**
  - **Pat One** - Old done item (done)
  - **Pat One** - Ongoing item (in progress)
`
	if err := os.WriteFile(filepath.Join(dir, "TEAMX_20260202.md"), []byte(prev), 0644); err != nil {
		t.Fatalf("write previous report: %v", err)
	}

	cfg := Config{
		ReportOutputDir: dir,
		TeamName:        "TEAMX",
	}

	orig := classifySectionsFn
	classifySectionsFn = func(_ Config, items []WorkItem, _ []sectionOption, _ []existingItemContext, _ []ClassificationCorrection) (map[int64]LLMSectionDecision, LLMUsage, error) {
		out := make(map[int64]LLMSectionDecision)
		for _, item := range items {
			out[item.ID] = LLMSectionDecision{
				SectionID:        "S0_0",
				NormalizedStatus: normalizeStatus(item.Status),
				Confidence:       0.95,
			}
		}
		return out, LLMUsage{}, nil
	}
	defer func() { classifySectionsFn = orig }()

	items := []WorkItem{
		{ID: 11, Author: "Pat Two", Description: "Old done item", Status: "done"},
		{ID: 12, Author: "Pat Three", Description: "New testing item", Status: "in test"},
		{ID: 13, Author: "Pat Four", Description: "New progress item", Status: "in progress"},
	}

	result, err := BuildReportsFromLast(cfg, items, mustDate(t, "20260209"), nil)
	if err != nil {
		t.Fatalf("BuildReportsFromLast failed: %v", err)
	}
	merged := result.Template

	team := renderTeamMarkdown(merged)

	boss := renderBossMarkdown(merged)

	idxDone := strings.Index(team, "Old done item (done)")
	idxTesting := strings.Index(team, "New testing item (in testing)")
	idxOldProgress := strings.Index(team, "Ongoing item (in progress)")
	idxNewProgress := strings.Index(team, "New progress item (in progress)")
	if !(idxDone >= 0 && idxTesting >= 0 && idxOldProgress >= 0 && idxNewProgress >= 0) {
		t.Fatalf("missing expected items in team report:\n%s", team)
	}
	if !(idxDone < idxTesting && idxTesting < idxOldProgress && idxOldProgress < idxNewProgress) {
		t.Fatalf("status ordering is incorrect in team report:\n%s", team)
	}
	if strings.Contains(team, "**Pat One** - Old done item (done)") {
		t.Fatalf("old done item from previous report should have been removed before merge:\n%s", team)
	}
	if !strings.Contains(boss, "Top Focus (") || !strings.Contains(boss, "Pat One") || !strings.Contains(boss, "Pat Two") || !strings.Contains(boss, "Pat Three") || !strings.Contains(boss, "Pat Four") {
		t.Fatalf("boss category heading should include authors:\n%s", boss)
	}
	if strings.Contains(boss, "**Pat Two** -") {
		t.Fatalf("boss report should not include author prefixes in item lines:\n%s", boss)
	}
}

func TestBuildReportsFromLast_LLMConfidenceAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	prev := `### TEAMX 20260202

#### Top Focus

- **Feature A**
  - **Pat One** - Existing ongoing item (in progress)
`
	if err := os.WriteFile(filepath.Join(dir, "TEAMX_20260202.md"), []byte(prev), 0644); err != nil {
		t.Fatalf("write previous report: %v", err)
	}

	cfg := Config{
		ReportOutputDir: dir,
		TeamName:        "TEAMX",
	}

	orig := classifySectionsFn
	classifySectionsFn = func(_ Config, items []WorkItem, _ []sectionOption, existing []existingItemContext, _ []ClassificationCorrection) (map[int64]LLMSectionDecision, LLMUsage, error) {
		out := make(map[int64]LLMSectionDecision)
		var dupKey string
		for _, ex := range existing {
			if strings.Contains(ex.Description, "Existing ongoing item") {
				dupKey = ex.Key
				break
			}
		}
		for _, item := range items {
			switch item.ID {
			case 21:
				out[item.ID] = LLMSectionDecision{
					SectionID:        "S0_0",
					NormalizedStatus: "in test",
					DuplicateOf:      dupKey,
					Confidence:       0.95,
				}
			case 22:
				out[item.ID] = LLMSectionDecision{
					SectionID:        "S0_0",
					NormalizedStatus: "done",
					Confidence:       0.40,
				}
			}
		}
		return out, LLMUsage{}, nil
	}
	defer func() { classifySectionsFn = orig }()

	items := []WorkItem{
		{ID: 21, Author: "Pat Two", Description: "Refined wording of existing ongoing item", Status: "in progress"},
		{ID: 22, Author: "Pat Three", Description: "Low confidence placement", Status: "in progress"},
	}

	result, err := BuildReportsFromLast(cfg, items, mustDate(t, "20260209"), nil)
	if err != nil {
		t.Fatalf("BuildReportsFromLast failed: %v", err)
	}
	merged := result.Template

	team := renderTeamMarkdown(merged)

	if !strings.Contains(team, "(in testing)") {
		t.Fatalf("duplicate merge should update status via normalized_status:\n%s", team)
	}
	if strings.Count(team, "(in testing)") != 1 {
		t.Fatalf("duplicate should not create a second testing item:\n%s", team)
	}
	if !strings.Contains(team, "#### Undetermined") || !strings.Contains(team, "Low confidence placement") {
		t.Fatalf("low-confidence decision should route to Undetermined:\n%s", team)
	}
	if !strings.Contains(team, "Low confidence placement (in progress)") {
		t.Fatalf("low-confidence decision should keep incoming status:\n%s", team)
	}
}

func TestBuildReportsFromLast_PreservesPrefixBlocks(t *testing.T) {
	dir := t.TempDir()
	prev := `### Product Alpha - 20260130

- **Observability stack design (in progress)**

#### Top Focus

- **Feature A**
  - **Pat One** - Existing ongoing item (in progress)
`
	if err := os.WriteFile(filepath.Join(dir, "TEAMX_20260202.md"), []byte(prev), 0644); err != nil {
		t.Fatalf("write previous report: %v", err)
	}

	cfg := Config{
		ReportOutputDir: dir,
		TeamName:        "TEAMX",
	}

	orig := classifySectionsFn
	classifySectionsFn = func(_ Config, items []WorkItem, _ []sectionOption, _ []existingItemContext, _ []ClassificationCorrection) (map[int64]LLMSectionDecision, LLMUsage, error) {
		out := make(map[int64]LLMSectionDecision)
		for _, item := range items {
			out[item.ID] = LLMSectionDecision{
				SectionID:        "S0_0",
				NormalizedStatus: normalizeStatus(item.Status),
				Confidence:       0.95,
			}
		}
		return out, LLMUsage{}, nil
	}
	defer func() { classifySectionsFn = orig }()

	result, err := BuildReportsFromLast(cfg, []WorkItem{{ID: 31, Author: "Pat Two", Description: "New item", Status: "in progress"}}, mustDate(t, "20260209"), nil)
	if err != nil {
		t.Fatalf("BuildReportsFromLast failed: %v", err)
	}
	merged := result.Template

	team := renderTeamMarkdown(merged)

	if !strings.Contains(team, "### Product Alpha - 20260130") {
		t.Fatalf("expected prefix block heading to be preserved:\n%s", team)
	}
	if !strings.Contains(team, "Observability stack design") {
		t.Fatalf("expected prefix block body to be preserved:\n%s", team)
	}
}

func TestFormatItemDescriptionCapitalization(t *testing.T) {
	itemWithAuthor := TemplateItem{
		Author:      "taylor stone",
		Description: `set heavyInfoLogDbTemplate to "not required"`,
		Status:      "in progress",
	}
	gotTeam := formatTeamItem(itemWithAuthor)
	wantTeam := `**Taylor Stone** - Set heavyInfoLogDbTemplate to "not required" (in progress)`
	if gotTeam != wantTeam {
		t.Fatalf("unexpected team item:\nwant: %s\ngot:  %s", wantTeam, gotTeam)
	}

	itemWithTicket := TemplateItem{
		Description: "improve data balance check warning messages",
		TicketIDs:   "1238836",
		Status:      "in progress",
	}
	gotBoss := formatBossItem(itemWithTicket)
	wantBoss := "[1238836] Improve data balance check warning messages (in progress)"
	if gotBoss != wantBoss {
		t.Fatalf("unexpected boss item:\nwant: %s\ngot:  %s", wantBoss, gotBoss)
	}

	if got := synthesizeName("River Chen (Alias)"); got != "River Chen" {
		t.Fatalf("expected alias removed from name, got: %s", got)
	}
}

func TestMergeCategoryHeadingAuthors(t *testing.T) {
	got := mergeCategoryHeadingAuthors(
		"Data Services (Casey, Quinn) (Casey Lane, Skyler Park)",
		[]string{"Casey Lane", "Skyler Park"},
	)
	want := "Data Services (Casey Lane, Skyler Park, Quinn)"
	if got != want {
		t.Fatalf("unexpected merged heading:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestMergeCategoryHeadingAuthors_StripsAliases(t *testing.T) {
	got := mergeCategoryHeadingAuthors(
		"Query Service (River Chen (Alias), Devon Hart)",
		[]string{"River Chen (Alias)", "Devon Hart"},
	)
	want := "Query Service (River Chen, Devon Hart)"
	if got != want {
		t.Fatalf("unexpected merged heading with aliases:\nwant: %s\ngot:  %s", want, got)
	}
}

func TestBuildReportsFromLast_PreservesMidTopHeading(t *testing.T) {
	dir := t.TempDir()
	prev := `### Product Alpha

#### Top Focus

- **Feature A**
  - **Pat One** - Existing ongoing item (in progress)

### Product Beta

#### Release and Support

- **Support Cases**
  - **Pat Two** - Existing support item (in progress)
`
	if err := os.WriteFile(filepath.Join(dir, "TEAMX_20260202.md"), []byte(prev), 0644); err != nil {
		t.Fatalf("write previous report: %v", err)
	}

	cfg := Config{
		ReportOutputDir: dir,
		TeamName:        "TEAMX",
	}

	orig := classifySectionsFn
	classifySectionsFn = func(_ Config, items []WorkItem, _ []sectionOption, _ []existingItemContext, _ []ClassificationCorrection) (map[int64]LLMSectionDecision, LLMUsage, error) {
		out := make(map[int64]LLMSectionDecision)
		for _, item := range items {
			out[item.ID] = LLMSectionDecision{
				SectionID:        "S0_0",
				NormalizedStatus: normalizeStatus(item.Status),
				Confidence:       0.95,
			}
		}
		return out, LLMUsage{}, nil
	}
	defer func() { classifySectionsFn = orig }()

	result, err := BuildReportsFromLast(cfg, []WorkItem{{ID: 99, Author: "Pat Five", Description: "New item", Status: "in progress"}}, mustDate(t, "20260209"), nil)
	if err != nil {
		t.Fatalf("BuildReportsFromLast failed: %v", err)
	}
	merged := result.Template

	team := renderTeamMarkdown(merged)

	if !strings.Contains(team, "### Product Alpha") {
		t.Fatalf("expected first top heading to be preserved:\n%s", team)
	}
	if !strings.Contains(team, "### Product Beta") {
		t.Fatalf("expected mid top heading to be preserved:\n%s", team)
	}
}

func mustDate(t *testing.T, ymd string) time.Time {
	t.Helper()
	d, err := time.Parse("20060102", ymd)
	if err != nil {
		t.Fatalf("invalid date %s: %v", ymd, err)
	}
	return d
}
