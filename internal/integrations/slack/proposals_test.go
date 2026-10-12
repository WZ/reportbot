package slackbot

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/proposals"
	sqlitedb "reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

type fakeProposals struct {
	mu        sync.Mutex
	proposed  int
	result    []proposals.StoredProposal
	approved  []int64
	rejected  []int64
	decideErr error
	done      chan struct{}
}

func (f *fakeProposals) ProposeStructure(ctx context.Context) ([]proposals.StoredProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposed++
	if f.done != nil {
		close(f.done)
		f.done = nil
	}
	return f.result, nil
}

func (f *fakeProposals) ApproveProposal(id int64, actor string) (proposals.StoredProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id)
	return proposals.StoredProposal{ID: id, StructureProposal: llm.StructureProposal{Kind: llm.ProposeRetire, SectionID: 9}}, f.decideErr
}

func (f *fakeProposals) AfterPublish() { _, _ = f.ProposeStructure(context.Background()) }

func (f *fakeProposals) RejectProposal(id int64, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, id)
	return f.decideErr
}

func withProposals(t *testing.T, p Proposer) {
	t.Helper()
	old := proposer
	proposer = p
	t.Cleanup(func() { proposer = old })
}

var mgrCfg = Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}, LLMProvider: "openai"}

func TestRetrospectOnlyGeneratesProposals(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	f := &fakeProposals{result: []proposals.StoredProposal{{ID: 7, Affected: 5,
		StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "Telemetry"}}}}
	cmd := slack.SlashCommand{Command: "/retrospect", ChannelID: "C1"}

	withProposals(t, f)
	cmd.UserID = "U1"
	handleRetrospective(api, db, mgrCfg, cmd)
	if f.proposed != 0 || !strings.Contains(rec.lastText("chat.postEphemeral"), "only managers") {
		t.Fatal("members cannot run /retrospect")
	}
	cmd.UserID = "UMGR"
	handleRetrospective(api, db, mgrCfg, cmd)
	if got := rec.lastText("chat.postEphemeral"); f.proposed != 1 || !strings.Contains(got, "Telemetry") || !strings.Contains(got, "5 Work Items") {
		t.Fatalf("/retrospect generates and lists Structure Proposals: %s", got)
	}

	withProposals(t, nil)
	handleRetrospective(api, db, Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}}, cmd)
	if !strings.Contains(rec.lastText("chat.postEphemeral"), "LLM isn't configured") {
		t.Fatal("without an LLM /retrospect says so")
	}
}

func TestProposalButtonsAreManagerOnlyAndOnce(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	f := &fakeProposals{}
	withProposals(t, f)

	handleBlockActions(api, db, mgrCfg, interaction("U1", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	if len(f.approved) != 0 {
		t.Fatal("members cannot approve proposals")
	}
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalReject, "8", rec.srv.URL+"/respond"))
	if len(f.approved) != 1 || f.approved[0] != 7 || len(f.rejected) != 1 || f.rejected[0] != 8 {
		t.Fatalf("manager buttons: %+v", f)
	}
	f.decideErr = proposals.ErrAlreadyDecided
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	if r := lastResponse(rec); !strings.Contains(r, "already decided") {
		t.Fatalf("approve twice: %s", r)
	}
	f.decideErr = proposals.ErrProposalStale
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalApprove, "9", rec.srv.URL+"/respond"))
	if r := lastResponse(rec); !strings.Contains(r, "Open Work Items") || !strings.Contains(r, "Not applied") {
		t.Fatalf("a stale retire is refused with a reason: %s", r)
	}
}

func lastResponse(rec *slackRecorder) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.responses) == 0 {
		return ""
	}
	t, _ := rec.responses[len(rec.responses)-1]["text"].(string)
	return t
}

func TestProposalBlocksHaveUniqueActions(t *testing.T) {
	tax := domain.Taxonomy{Nodes: []domain.TaxonomyNode{{ID: 1, Level: domain.LevelArea, Name: "Product A"}}}
	text, blocks := ProposalBlocks(tax, []proposals.StoredProposal{
		{ID: 7, Affected: 5, StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "Telemetry", Description: "Metrics"}},
	})
	b := blocksJSON(blocks)
	if !strings.Contains(text, "1 Structure Proposal") || !strings.Contains(b, "Telemetry") || !strings.Contains(b, "5 Work Items") ||
		!strings.Contains(b, actionProposalApprove) || !strings.Contains(b, actionProposalReject) {
		t.Fatalf("proposal DM: %s %s", text, b)
	}
}

func TestFallbackNoteOffersChange(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	n := NewNotifier(api, db, mgrCfg)
	item := domain.WorkItem{ID: 5, Description: "rack fan", AuthorID: "U1", Status: domain.StatusInProgress}
	n.FiledByFallback(item, domain.Section{ID: 9, Area: "Product A", Category: "Infrastructure"}, false)
	got := rec.lastText("chat.postMessage")
	if !strings.Contains(got, "Filed under *Infrastructure* automatically (no answer within a day)") ||
		!strings.Contains(got, actionPlaceOther) || !strings.Contains(got, "Change") {
		t.Fatalf("the reporter is told and can change it: %s", got)
	}
	n.FiledByFallback(item, domain.Section{ID: 9, Area: "Product A", Category: "Infrastructure"}, true)
	if got := rec.lastText("chat.postMessage"); !strings.Contains(got, "before the report was published") {
		t.Fatalf("a publish-time fallback says so: %s", got)
	}
}

func TestPublishFallbackRunsOnlyAfterConfirmation(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	cm, infra := seedSections(t, db)
	cfg := mgrCfg
	cfg.MondayCutoffTime, cfg.ReportOutputDir, cfg.TeamName = "12:00", t.TempDir(), "T"
	id, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "unsure", Author: "Dev A", AuthorID: "U1", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: inReportWeek(cfg), FilingState: domain.FilingAsking, UndeterminedReason: domain.ReasonLowConfidence})
	if err := sqlitedb.InsertJudgments(db, []sqlitedb.Judgment{{WorkItemID: id, Kind: "section_choice", Model: "m",
		AnswerJSON: `{"probabilities":{"` + domain.SectionKey(infra) + `":0.5,"` + domain.SectionKey(cm) + `":0.3}}`}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeProposals{done: make(chan struct{})}
	done := f.done
	withProposals(t, f)

	handleGenerateReport(api, db, cfg, slack.SlashCommand{Command: "/generate-report", UserID: "UMGR", ChannelID: "C1"})
	warning := rec.lastText("chat.postEphemeral")
	if !strings.Contains(warning, "still in Undetermined") || !strings.Contains(warning, "first choice") {
		t.Fatalf("the manager is warned first, and told what publishing will do: %s", warning)
	}
	if it, _ := sqlitedb.GetWorkItemByID(db, id); it.SectionID != 0 || rec.count("chat.postMessage") != 0 {
		t.Fatalf("nothing is filed and nobody is DMed before the manager confirms: %+v", it)
	}

	// Publish anyway: the fallback runs, then the report is published.
	handleBlockActions(api, db, cfg, interaction("UMGR", actionPublishAnyway, "team|false", ""))
	if it, _ := sqlitedb.GetWorkItemByID(db, id); it.SectionID != infra || it.PlacedBy != domain.PlacedByFallback {
		t.Fatalf("after confirming, the unanswered item is filed under the model's #1: %+v", it)
	}
	if rec.count("chat.postMessage") == 0 {
		t.Fatal("the reporter is told after the manager confirms")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM published_reports`).Scan(&n)
	if n != 1 {
		t.Fatal("the report is published after confirmation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Structure Proposals are generated after publishing")
	}
}

func TestStatsCountsFallbackAndSaysWhenProposalsAreOff(t *testing.T) {
	st := WeekStats{ItemsThisWeek: 10, FiledByModel: 4, Asked: 6, FiledByFallback: 2, PersonCorrections: map[int64]int{}}
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	on := formatStats(st, domain.Taxonomy{}, monday, monday.AddDate(0, 0, 7), true)
	if !strings.Contains(on, "Filed automatically: reporter didn't answer: 2") || strings.Contains(on, "are off") {
		t.Fatalf("stats with the LLM: %s", on)
	}
	if off := formatStats(st, domain.Taxonomy{}, monday, monday.AddDate(0, 0, 7), false); !strings.Contains(off, "Structure Proposals are off") {
		t.Fatalf("stats without the LLM: %s", off)
	}
}

// blocksJSON keeps < and > as written, so tests can see raw Slack control sequences.
func blocksJSON(blocks []slack.Block) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(blocks)
	return b.String()
}

func TestUntrustedTextIsEscapedInProposalMessages(t *testing.T) {
	evil := "Racks & <!channel> <http://x.example|click>"
	ps := []proposals.StoredProposal{{ID: 7, StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "<!channel>",
		Description: evil, Reason: evil}}}
	tax := domain.Taxonomy{Nodes: []domain.TaxonomyNode{{ID: 1, Level: domain.LevelArea, Name: "<@U1> Area"}}}
	pText, pBlocks := ProposalBlocks(tax, ps)
	all := retrospectText(tax, ps) + pText + blocksJSON(pBlocks)
	for _, raw := range []string{"<!", "<@", "<http"} {
		if strings.Contains(all, raw) {
			t.Fatalf("untrusted text must be escaped; found %q in:\n%s", raw, all)
		}
	}
	if !strings.Contains(all, "&lt;!channel&gt;") {
		t.Fatalf("expected escaped text:\n%s", all)
	}
}

func TestFallbackAndFilingRepliesEscapeItemText(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	item := domain.WorkItem{ID: 5, Description: "fix <!here> <http://x.example|y>", AuthorID: "U1", Status: domain.StatusInProgress}
	NewNotifier(api, db, mgrCfg).FiledByFallback(item, domain.Section{ID: 9, Area: "Product A", Category: "<!channel>"}, false)
	if got := rec.lastText("chat.postMessage"); strings.Contains(got, "<!") || strings.Contains(got, "<http") {
		t.Fatalf("the fallback note must escape item text and Section names: %s", got)
	}
	_, blocks := FiledBlocks(mgrCfg, filing.Result{Item: item, Section: &domain.Section{ID: 9, Category: "<!channel>"}})
	if b := blocksJSON(blocks); strings.Contains(b, "<!") || strings.Contains(b, "<http") {
		t.Fatalf("filing replies must escape item text and Section names: %s", b)
	}
}

func TestSlackEditsUpdatesAndStatusChangesAreRecorded(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	cm, _ := seedSections(t, db)
	add := func(desc string) int64 {
		id, err := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Source: "slack",
			Status: domain.StatusInProgress, ReportedAt: time.Now(), SectionID: cm, FilingState: domain.FilingFiled, PlacedBy: domain.PlacedByModel})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	existing, report := add("shard timeout"), add("shard timeout fixed")
	handleBlockActions(api, db, mgrCfg, interaction("U1", actionUpdateConfirm, itoa(existing)+":"+itoa(report), rec.srv.URL+"/respond"))
	edits, _ := sqlitedb.GetWorkItemEdits(db, existing)
	if len(edits) != 1 || edits[0].Kind != sqlitedb.EditUpdateMerge || edits[0].ActorID != "U1" || edits[0].Via != "slack" || edits[0].RelatedItemID != report {
		t.Fatalf("Update merge from Slack: %+v", edits)
	}
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionCheckDone, itoa(existing), ""))
	edits, _ = sqlitedb.GetWorkItemEdits(db, existing)
	if len(edits) != 2 || edits[1].Kind != sqlitedb.EditStatus || edits[1].ActorID != "UMGR" || edits[1].After.Status != domain.StatusDone {
		t.Fatalf("Done from /check: %+v", edits)
	}
}
