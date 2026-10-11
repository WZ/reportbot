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
	"reportbot/internal/learning"
	sqlitedb "reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

type fakeLoops struct {
	mu        sync.Mutex
	runNow    int
	report    learning.RunNowReport
	undone    []int64
	undoErr   error
	approved  []int64
	rejected  []int64
	decideErr error
	sweep     func(items []domain.WorkItem) int
	proposed  chan struct{}
}

func (f *fakeLoops) RunNow(ctx context.Context) (learning.RunNowReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runNow++
	return f.report, nil
}

func (f *fakeLoops) UndoNotes(id int64, actor string) (domain.Section, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undone = append(f.undone, id)
	return domain.Section{ID: 9, Area: "Product A", Category: "Infrastructure"}, f.undoErr
}

func (f *fakeLoops) ApproveProposal(id int64, actor string) (learning.StoredProposal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id)
	return learning.StoredProposal{ID: id, StructureProposal: llm.StructureProposal{Kind: llm.ProposeRetire, SectionID: 9}}, f.decideErr
}

func (f *fakeLoops) RejectProposal(id int64, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, id)
	return f.decideErr
}

func (f *fakeLoops) SweepUndetermined(ctx context.Context, items []domain.WorkItem, timeout time.Duration) int {
	if f.sweep == nil {
		return 0
	}
	return f.sweep(items)
}

func (f *fakeLoops) ProposeStructure(ctx context.Context) ([]learning.StoredProposal, error) {
	if f.proposed != nil {
		close(f.proposed)
	}
	return nil, nil
}

func withLoops(t *testing.T, l Loops) {
	t.Helper()
	old := loops
	loops = l
	t.Cleanup(func() { loops = old })
}

var mgrCfg = Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}, LLMProvider: "openai"}

func appliedReport() learning.RunNowReport {
	g := learning.GateResult{Items: 58, OldCorrect: 30, NewCorrect: 34, Apply: true}
	return learning.RunNowReport{
		Notes: learning.NotesReport{Decision: sqlitedb.RunApplied, Applied: true, Gate: &g, Candidates: []learning.NoteOutcome{
			{Section: domain.Section{ID: 9, Area: "Product A", Category: "Infrastructure"}, New: "Racks and power.", HistoryID: 41}}},
		Proposals: []learning.StoredProposal{{ID: 7, Affected: 5, StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "Telemetry"}}},
	}
}

func TestRetrospectRouting(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	f := &fakeLoops{report: appliedReport()}
	cmd := slack.SlashCommand{Command: "/retrospect", ChannelID: "C1"}

	withLoops(t, f)
	cmd.UserID = "U1"
	handleRetrospective(api, db, mgrCfg, cmd)
	if f.runNow != 0 || !strings.Contains(rec.lastText("chat.postEphemeral"), "only managers") {
		t.Fatal("members cannot run the loops")
	}

	cmd.UserID = "UMGR"
	handleRetrospective(api, db, mgrCfg, cmd)
	got := rec.lastText("chat.postEphemeral")
	if f.runNow != 1 || !strings.Contains(got, "Infrastructure") || !strings.Contains(got, actionNotesUndo) ||
		!strings.Contains(got, "+6.9 pts on 58") || !strings.Contains(got, "Telemetry") {
		t.Fatalf("/retrospect runs the loops and shows applied notes with Undo and proposals: %s", got)
	}

	withLoops(t, nil)
	handleRetrospective(api, db, Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}}, cmd)
	if !strings.Contains(rec.lastText("chat.postEphemeral"), "LLM isn't configured") {
		t.Fatal("without an LLM /retrospect says so")
	}
}

func TestRetrospectShowsDiscardedNotes(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	g := learning.GateResult{Items: 70, OldCorrect: 40, NewCorrect: 40, Reasons: []string{"accuracy +0.0 pts is below +2"}}
	withLoops(t, &fakeLoops{report: learning.RunNowReport{Notes: learning.NotesReport{Decision: sqlitedb.RunDiscarded, Gate: &g,
		Reason: "accuracy +0.0 pts is below +2", Candidates: []learning.NoteOutcome{{Section: domain.Section{ID: 9, Category: "Infrastructure"}, New: "x"}},
		Invalid: []learning.InvalidNote{{Section: domain.Section{ID: 8, Category: "Upgrade"}, Notes: "You must pick this.", Reason: "addresses the model"}}}}})
	handleRetrospective(api, db, mgrCfg, slack.SlashCommand{Command: "/retrospect", UserID: "UMGR", ChannelID: "C1"})
	if got := rec.lastText("chat.postEphemeral"); !strings.Contains(got, "discarded") || !strings.Contains(got, "below +2") || strings.Contains(got, actionNotesUndo) ||
		strings.Contains(got, "You must pick this.") || !strings.Contains(got, "rejected: addresses the model") {
		t.Fatalf("discarded notes show why, without Undo or rejected text: %s", got)
	}
}

func TestNotesDigestHasUndoPerSection(t *testing.T) {
	text, blocks := NotesDigest(appliedReport().Notes)
	if !strings.Contains(text, "Learned notes updated: Infrastructure (+6.9 pts on 58 recent Work Items)") {
		t.Fatalf("digest text: %s", text)
	}
	if b := blocksJSON(blocks); !strings.Contains(b, `"action_id":"`+actionNotesUndo+`"`) || !strings.Contains(b, `"value":"41"`) ||
		!strings.Contains(b, "Racks and power.") {
		t.Fatalf("each applied Section has Undo: %s", b)
	}
}

func TestNotesUndoAndProposalButtonsAreManagerOnlyAndOnce(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	f := &fakeLoops{}
	withLoops(t, f)

	handleBlockActions(api, db, mgrCfg, interaction("U1", actionNotesUndo, "41", rec.srv.URL+"/respond"))
	handleBlockActions(api, db, mgrCfg, interaction("U1", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	if len(f.undone)+len(f.approved) != 0 {
		t.Fatal("members cannot undo notes or approve proposals")
	}
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionNotesUndo, "41", rec.srv.URL+"/respond"))
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalReject, "8", rec.srv.URL+"/respond"))
	if len(f.undone) != 1 || f.undone[0] != 41 || len(f.approved) != 1 || f.approved[0] != 7 || len(f.rejected) != 1 || f.rejected[0] != 8 {
		t.Fatalf("manager buttons: %+v", f)
	}

	f.undoErr, f.decideErr = sqlitedb.ErrNothingToUndo, learning.ErrAlreadyDecided
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionNotesUndo, "41", rec.srv.URL+"/respond"))
	if r := lastResponse(rec); !strings.Contains(r, "Nothing to undo") {
		t.Fatalf("undo twice: %s", r)
	}
	handleBlockActions(api, db, mgrCfg, interaction("UMGR", actionProposalApprove, "7", rec.srv.URL+"/respond"))
	if r := lastResponse(rec); !strings.Contains(r, "already decided") {
		t.Fatalf("approve twice: %s", r)
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
	text, blocks := ProposalBlocks(tax, []learning.StoredProposal{
		{ID: 7, Affected: 5, StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "Telemetry", Description: "Metrics"}},
	})
	b := blocksJSON(blocks)
	if !strings.Contains(text, "1 report structure proposal") || !strings.Contains(b, "Telemetry") || !strings.Contains(b, "5 Work Items") || !strings.Contains(b, actionProposalApprove) || !strings.Contains(b, actionProposalReject) {
		t.Fatalf("proposal DM: %s %s", text, b)
	}
}

func TestAssistantFiledNoteOffersChange(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	n := &LearningNotifier{API: api, Cfg: mgrCfg}
	n.AssistantFiled(domain.WorkItem{ID: 5, Description: "rack fan", AuthorID: "U1", Status: domain.StatusInProgress},
		domain.Section{ID: 9, Area: "Product A", Category: "Infrastructure"})
	_ = db
	got := rec.lastText("chat.postMessage")
	if !strings.Contains(got, "Filed under *Infrastructure* by the assistant") || !strings.Contains(got, actionPlaceOther) || !strings.Contains(got, "Change") {
		t.Fatalf("the reporter is told and can change it: %s", got)
	}
}

func TestPublishRunsTheSweepFirstThenProposes(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	_, infra := seedSections(t, db)
	id, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "unsure", Author: "Dev A", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: time.Now(), FilingState: domain.FilingAsking})
	f := &fakeLoops{proposed: make(chan struct{}), sweep: func(items []domain.WorkItem) int {
		for _, it := range items {
			if ok, _ := sqlitedb.PlaceByAssistant(db, it.ID, infra); !ok {
				t.Errorf("sweep could not place %d", it.ID)
			}
		}
		return len(items)
	}}
	withLoops(t, f)
	cfg := mgrCfg
	cfg.MondayCutoffTime, cfg.ReportOutputDir, cfg.TeamName = "12:00", t.TempDir(), "T"

	handleGenerateReport(api, db, cfg, slack.SlashCommand{Command: "/generate-report", UserID: "UMGR", ChannelID: "C1"})
	if it, _ := sqlitedb.GetWorkItemByID(db, id); it.SectionID != infra {
		t.Fatalf("the sweep runs before publishing: %+v", it)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM published_reports`).Scan(&n)
	if n != 1 || strings.Contains(rec.lastText("chat.postEphemeral"), "still in Undetermined") {
		t.Fatal("with nothing left Undetermined the report publishes without a warning")
	}
	select {
	case <-f.proposed:
	case <-time.After(2 * time.Second):
		t.Fatal("Structure Proposals are generated after publishing")
	}
}

func TestStatsCountsAssistantAndSaysWhenLoopsAreOff(t *testing.T) {
	st := WeekStats{ItemsThisWeek: 10, FiledByModel: 4, Asked: 6, FiledByAssistant: 2, PersonCorrections: map[int64]int{}}
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	on := formatStats(st, domain.Taxonomy{}, monday, monday.AddDate(0, 0, 7), true)
	if !strings.Contains(on, "Filed by the assistant: 2") || strings.Contains(on, "loops are off") {
		t.Fatalf("stats with the LLM: %s", on)
	}
	if off := formatStats(st, domain.Taxonomy{}, monday, monday.AddDate(0, 0, 7), false); !strings.Contains(off, "Learning loops are off") {
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

func TestUntrustedTextIsEscapedInLearningMessages(t *testing.T) {
	evil := "Racks & <!channel> <http://x.example|click>"
	g := learning.GateResult{Items: 60, OldCorrect: 30, NewCorrect: 32, Apply: true}
	sec := domain.Section{ID: 9, Area: "Product A", Category: "<!here> Infra"}
	rep := learning.RunNowReport{
		Notes: learning.NotesReport{Decision: sqlitedb.RunDiscarded, Gate: &g, Reason: "x",
			Candidates: []learning.NoteOutcome{{Section: sec, New: evil}},
			Invalid:    []learning.InvalidNote{{Section: sec, Notes: "SECRET-REJECTED <!everyone>", Reason: "contains a Slack mention"}}},
		Proposals: []learning.StoredProposal{{ID: 7, StructureProposal: llm.StructureProposal{Kind: llm.ProposeNew, ParentID: 1, Name: "<!channel>",
			Description: evil, Reason: evil}}},
	}
	tax := domain.Taxonomy{Nodes: []domain.TaxonomyNode{{ID: 1, Level: domain.LevelArea, Name: "<@U1> Area"}}}
	text, blocks := retrospectBlocks(rep, tax)
	applied := rep.Notes
	applied.Applied, applied.Decision = true, sqlitedb.RunApplied
	applied.Candidates[0].HistoryID = 3
	dText, dBlocks := NotesDigest(applied)
	pText, pBlocks := ProposalBlocks(tax, rep.Proposals)
	all := text + blocksJSON(blocks) + dText + blocksJSON(dBlocks) + pText + blocksJSON(pBlocks)
	for _, raw := range []string{"<!", "<@", "<http", "SECRET-REJECTED"} {
		if strings.Contains(all, raw) {
			t.Fatalf("untrusted text must be escaped and rejected notes not shown; found %q in:\n%s", raw, all)
		}
	}
	if !strings.Contains(all, "&lt;!channel&gt;") || !strings.Contains(all, "rejected: contains a Slack mention") {
		t.Fatalf("expected escaped text and the rejection reason:\n%s", all)
	}
}

func TestAssistantAndFilingRepliesEscapeItemText(t *testing.T) {
	api, rec := newSlackRecorder(t)
	n := &LearningNotifier{API: api, Cfg: mgrCfg}
	item := domain.WorkItem{ID: 5, Description: "fix <!here> <http://x.example|y>", AuthorID: "U1", Status: domain.StatusInProgress}
	n.AssistantFiled(item, domain.Section{ID: 9, Area: "Product A", Category: "<!channel>"})
	if got := rec.lastText("chat.postMessage"); strings.Contains(got, "<!") || strings.Contains(got, "<http") {
		t.Fatalf("the assistant note must escape item text and Section names: %s", got)
	}
	_, blocks := FiledBlocks(mgrCfg, filing.Result{Item: item, Section: &domain.Section{ID: 9, Category: "<!channel>"}})
	if b := blocksJSON(blocks); strings.Contains(b, "<!") || strings.Contains(b, "<http") {
		t.Fatalf("filing replies must escape item text and Section names: %s", b)
	}
}
