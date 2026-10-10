package report

import (
	"strings"
	"testing"
	"time"

	"reportbot/internal/domain"
)

// testTaxonomy: Area "Product A" with Category "Release" (Sections 3 "7.6.3", 4 "7.4.7"),
// Category "Infrastructure" (id 5, itself a Section) and a retired Section 6.
func testTaxonomy() domain.Taxonomy {
	return domain.Taxonomy{Nodes: []domain.TaxonomyNode{
		{ID: 1, Level: domain.LevelArea, Name: "Product A", SortOrder: 10},
		{ID: 2, ParentID: 1, Level: domain.LevelCategory, Name: "Release", SortOrder: 10},
		{ID: 3, ParentID: 2, Level: domain.LevelSection, Name: "7.6.3", SortOrder: 10},
		{ID: 4, ParentID: 2, Level: domain.LevelSection, Name: "7.4.7", SortOrder: 20},
		{ID: 6, ParentID: 2, Level: domain.LevelSection, Name: "Old", SortOrder: 30, Retired: true},
		{ID: 5, ParentID: 1, Level: domain.LevelCategory, Name: "Infrastructure", SortOrder: 20},
	}}
}

func TestBuildViewPlacesItemsAndUndetermined(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	items := []WorkItem{
		{ID: 1, Author: "dev a", Description: "done thing", Status: domain.StatusDone, SectionID: 3, ReportedAt: base},
		{ID: 2, Author: "dev b", Description: "ongoing thing", Status: domain.StatusInProgress, SectionID: 3, ReportedAt: base.Add(time.Hour)},
		{ID: 3, Author: "dev a", Description: "rack power", Status: domain.StatusBlocked, SectionID: 5, ReportedAt: base},
		{ID: 4, Author: "dev c", Description: "unsure", Status: domain.StatusInProgress, ReportedAt: base},
		{ID: 5, Author: "dev c", Description: "retired home", Status: domain.StatusInTesting, SectionID: 6, ReportedAt: base},
		{ID: 6, Author: "dev c", Description: "category with sections", Status: domain.StatusDone, SectionID: 2, ReportedAt: base},
	}
	v := BuildView(testTaxonomy(), items)
	if len(v.Areas) != 1 || len(v.Areas[0].Categories) != 2 {
		t.Fatalf("unexpected structure: %+v", v)
	}
	release := v.Areas[0].Categories[0]
	if len(release.Sections) != 1 || release.Sections[0].Section.ID != 3 {
		t.Fatalf("empty Sections must be omitted: %+v", release.Sections)
	}
	if got := release.Sections[0].Items; got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("expected in progress before done, got %d,%d", got[0].ID, got[1].ID)
	}
	if infra := v.Areas[0].Categories[1]; len(infra.Items) != 1 || infra.Items[0].ID != 3 {
		t.Fatalf("expected Category-as-Section items: %+v", infra)
	}
	if len(v.Undetermined) != 3 || v.ItemCount() != 6 {
		t.Fatalf("expected unplaced, retired and non-fileable items in Undetermined: %+v", v.Undetermined)
	}
}

func TestRenderTeamAndBossMarkdown(t *testing.T) {
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	items := []WorkItem{
		{ID: 1, Author: "dev a", Description: "ship build", Status: domain.StatusDone, SectionID: 3, ReportedAt: base, TicketIDs: "1234567"},
		{ID: 2, Author: "dev b", Description: "rack power", Status: domain.StatusInProgress, StatusNote: "waiting on vendor", SectionID: 5, ReportedAt: base},
		{ID: 3, Author: "dev c", Description: "unsure", Status: domain.StatusInProgress, ReportedAt: base},
	}
	v := BuildView(testTaxonomy(), items)
	team := RenderTeamMarkdown(v)
	want := `### Product A

#### Release

- **7.6.3**
  - **Dev A** - [1234567] Ship build (done)

#### Infrastructure

- **Dev B** - Rack power (in progress: waiting on vendor)

#### Undetermined

- **Dev C** - Unsure (in progress)
`
	if team != want {
		t.Fatalf("unexpected team report:\n%s\nwant:\n%s", team, want)
	}
	boss := RenderBossMarkdown(v)
	if !strings.Contains(boss, "#### Release (Dev A)") || !strings.Contains(boss, "  - [1234567] Ship build (done)") {
		t.Fatalf("unexpected boss report:\n%s", boss)
	}
	if RenderMarkdownByMode(v, "boss") != boss || RenderMarkdownByMode(v, "team") != team {
		t.Fatal("RenderMarkdownByMode must dispatch by mode")
	}
}

func TestStatusTextPendingAndNote(t *testing.T) {
	if got := StatusText(WorkItem{}); got != "filing…" {
		t.Fatalf("unexpected pending status text %q", got)
	}
	if got := StatusText(WorkItem{Status: "done", StatusNote: "Done"}); got != "done" {
		t.Fatalf("a note equal to the status adds nothing, got %q", got)
	}
}

func TestFormatItemDescriptionCapitalization(t *testing.T) {
	itemWithAuthor := WorkItem{
		Author:      "taylor stone",
		Description: `set heavyInfoLogDbTemplate to "not required"`,
		Status:      "in progress",
	}
	gotTeam := FormatTeamItem(itemWithAuthor)
	wantTeam := `**Taylor Stone** - Set heavyInfoLogDbTemplate to "not required" (in progress)`
	if gotTeam != wantTeam {
		t.Fatalf("unexpected team item:\nwant: %s\ngot:  %s", wantTeam, gotTeam)
	}

	itemWithTicket := WorkItem{
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

func TestFormatItemDedupesLeadingTicketPrefix(t *testing.T) {
	teamItem := WorkItem{
		Author:      "Jordan Hart",
		Description: "[7004001] [7004001] build cache index for snapshot restore",
		TicketIDs:   "7004001",
		Status:      "done",
	}
	gotTeam := FormatTeamItem(teamItem)
	wantTeam := "**Jordan Hart** - [7004001] Build cache index for snapshot restore (done)"
	if gotTeam != wantTeam {
		t.Fatalf("unexpected deduped team item:\nwant: %s\ngot:  %s", wantTeam, gotTeam)
	}

	bossItem := WorkItem{
		Description: "[7004002] [7004002] replace worker queue retry policy",
		TicketIDs:   "7004002",
		Status:      "in progress",
	}
	gotBoss := formatBossItem(bossItem)
	wantBoss := "[7004002] Replace worker queue retry policy (in progress)"
	if gotBoss != wantBoss {
		t.Fatalf("unexpected deduped boss item:\nwant: %s\ngot:  %s", wantBoss, gotBoss)
	}

	plainPrefix := WorkItem{
		Author:      "Marik Hsiao",
		Description: "1259566 added consul gossip encryption support",
		TicketIDs:   "1259566",
		Status:      "done",
	}
	gotPlain := FormatTeamItem(plainPrefix)
	wantPlain := "**Marik Hsiao** - [1259566] Added consul gossip encryption support (done)"
	if gotPlain != wantPlain {
		t.Fatalf("unexpected deduped plain-prefix item:\nwant: %s\ngot:  %s", wantPlain, gotPlain)
	}

	bracketedTicketIDs := WorkItem{
		Author:      "Howard Shen",
		Description: "[1201950] make up siem MVs prepare test document and assist QA on testing",
		TicketIDs:   "[1201950]",
		Status:      "in progress",
	}
	gotBracketed := FormatTeamItem(bracketedTicketIDs)
	wantBracketed := "**Howard Shen** - [1201950] Make up siem MVs prepare test document and assist QA on testing (in progress)"
	if gotBracketed != wantBracketed {
		t.Fatalf("unexpected bracketed ticket normalization:\nwant: %s\ngot:  %s", wantBracketed, gotBracketed)
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
