package slackbot

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
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

	free, err := parseReportItems("[1234567] Fix crash (root cause analysis in progress)\nNo status here", "Alice", loc)
	if err != nil {
		t.Fatal(err)
	}
	if free[0].Status != "" || free[0].StatusNote != "root cause analysis in progress" || free[0].TicketIDs != "1234567" {
		t.Fatalf("free text leaves Status to the decision model and keeps the note: %+v", free[0])
	}
	if free[1].Status != "" || free[1].StatusNote != "" {
		t.Fatalf("no status text: no default, the model decides: %+v", free[1])
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
		{name: "post channel", input: "post", wantMode: "post", wantPrivate: false},
		{name: "post private", input: "post private", wantMode: "post", wantPrivate: true},
		{name: "private only", input: "private", wantMode: "team", wantPrivate: true},
		{name: "boss channel", input: "boss channel", wantMode: "boss", wantPrivate: false},
		{name: "conflicting modes", input: "post boss", wantErr: true},
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

func TestResolveNudgeTarget(t *testing.T) {
	resetUserCacheForTest(t)
	api := slack.New(
		"xoxb-test",
		slack.OptionAPIURL("https://slack.test/api/"),
		slack.OptionHTTPClient(&http.Client{
			Transport: mockSlackRoundTripper(func(req *http.Request) (*http.Response, error) {
				form, err := mockSlackRequestForm(req)
				if err != nil {
					return nil, err
				}
				switch strings.TrimPrefix(req.URL.Path, "/api/") {
				case "users.info":
					userID := form.Get("user")
					return mockSlackJSONResponse(req, map[string]any{
						"ok": true,
						"user": map[string]any{
							"id":        userID,
							"name":      "pat",
							"real_name": "Pat Example",
							"profile": map[string]any{
								"display_name": "Pat Example",
							},
						},
					})
				case "users.list":
					return mockSlackJSONResponse(req, map[string]any{
						"ok": true,
						"members": []map[string]any{
							{
								"id":        "U0PAT1234",
								"name":      "pat",
								"real_name": "Pat Example",
								"profile": map[string]any{
									"display_name": "Pat Example",
								},
							},
						},
					})
				default:
					return mockSlackJSONResponse(req, map[string]any{"ok": true})
				}
			}),
		}),
	)

	cfg := Config{TeamMembers: []string{"Pat Example"}}

	id, label, err := resolveNudgeTarget(api, cfg, "Pat")
	if err != nil {
		t.Fatalf("resolveNudgeTarget by name failed: %v", err)
	}
	if id != "U0PAT1234" || label != "Pat Example" {
		t.Fatalf("unexpected name resolution: id=%q label=%q", id, label)
	}

	id, label, err = resolveNudgeTarget(api, cfg, "U0PAT1234")
	if err != nil {
		t.Fatalf("resolveNudgeTarget by ID failed: %v", err)
	}
	if id != "U0PAT1234" || label != "Pat Example" {
		t.Fatalf("unexpected ID resolution: id=%q label=%q", id, label)
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

func TestFormatListItemText_AddsLineNumber(t *testing.T) {
	item := WorkItem{
		Author:      "Alex Rivera",
		Description: "Tune query timeout for widget pipeline",
		Status:      "in progress",
		TicketIDs:   "7003001",
	}

	got := formatListItemText(7, item, " [GitLab]", " _Support Cases_")
	item.StatusNote = "waiting on vendor"
	if got := formatListItemText(1, item, "", ""); !strings.Contains(got, "(in progress: waiting on vendor)") {
		t.Fatalf("expected the Status Note in the list line: %q", got)
	}
	want := "7. *Alex Rivera*: [7003001] Tune query timeout for widget pipeline (in progress) [GitLab] _Support Cases_"
	if got != want {
		t.Fatalf("formatListItemText() = %q, want %q", got, want)
	}
}

func TestParseListScope(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "", want: listScopeMine},
		{input: "mine", want: listScopeMine},
		{input: "all", want: listScopeAll},
		{input: " ALL ", want: listScopeAll},
		{input: "team", wantErr: true},
	}

	for _, tt := range tests {
		got, err := parseListScope(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("parseListScope(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseListScope(%q) error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("parseListScope(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMemberReportedThisWeek(t *testing.T) {
	reportedIDs := map[string]bool{"U123": true}
	reportedAuthors := []string{"Alex Rivera", "Jordan Patel"}

	if !memberReportedThisWeek("U123", []string{"Alex Rivera"}, reportedIDs, reportedAuthors) {
		t.Fatal("expected member to be reported when author_id is present")
	}
	if !memberReportedThisWeek("U999", []string{"Alex"}, map[string]bool{}, reportedAuthors) {
		t.Fatal("expected fuzzy name match against fetched author to count as reported")
	}
	if memberReportedThisWeek("U999", []string{"Taylor"}, map[string]bool{}, reportedAuthors) {
		t.Fatal("expected unmatched member name to be considered missing")
	}
}

func TestFindLatestTeamReportFile(t *testing.T) {
	dir := t.TempDir()
	teamName := "Demo Team"

	files := map[string]string{
		"Demo Team_20260214.md":  "old",
		"Demo Team_20260221.md":  "new",
		"Demo Team_20260221.eml": "ignore-eml",
		"Other_20260228.md":      "ignore-other-team",
		"Demo Team_latest.md":    "ignore-invalid-date",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("write test file %s: %v", name, err)
		}
	}

	path, date, err := findLatestTeamReportFile(dir, teamName)
	if err != nil {
		t.Fatalf("findLatestTeamReportFile error: %v", err)
	}
	if filepath.Base(path) != "Demo Team_20260221.md" {
		t.Fatalf("unexpected latest file: %s", path)
	}
	if got := date.Format("20060102"); got != "20260221" {
		t.Fatalf("unexpected latest date: %s", got)
	}
}

func TestFindLatestTeamReportFile_NoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "OtherTeam_20260221.md"), []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, _, err := findLatestTeamReportFile(dir, "Demo Team")
	if err == nil {
		t.Fatal("expected error when no matching team report exists")
	}
}
