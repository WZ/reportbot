package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/filing"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/storage/sqlite"
)

type recorder struct {
	mu        sync.Mutex
	notes     []NotesReport
	proposals [][]StoredProposal
	assistant []string
}

func (r *recorder) NotesApplied(rep NotesReport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, rep)
}

func (r *recorder) ProposalsReady(ps []StoredProposal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proposals = append(r.proposals, ps)
}

func (r *recorder) AssistantFiled(item domain.WorkItem, s domain.Section) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assistant = append(r.assistant, fmt.Sprintf("%d:%s", item.ID, s.Label()))
}

type env struct {
	db       *sql.DB
	l        *Learner
	rec      *recorder
	sec      map[string]int64
	gwCalls  atomic.Int32
	llmCalls atomic.Int32
	llmText  func(input string) string
	llmBlock chan struct{} // when set, the LLM waits for it or for the request to end
}

func choice(probs map[string]float64) map[string]any {
	best, bestP := "", -1.0
	for k, p := range probs {
		if p > bestP || (p == bestP && k < best) {
			best, bestP = k, p
		}
	}
	return map[string]any{"type": "choice", "choice": best, "probabilities": probs, "confidence": bestP}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "learning.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ids, err := sqlite.SeedTaxonomy(db, []sqlite.SeedArea{{Name: "Product A", Categories: []sqlite.SeedCategory{
		{Name: "Engineering", Sections: []string{"Cluster Manager", "Upgrade"}},
		{Name: "Infrastructure"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, rec: &recorder{}, sec: map[string]int64{
		"cluster": ids[sqlite.SeedKey("Product A", "Engineering", "Cluster Manager")],
		"upgrade": ids[sqlite.SeedKey("Product A", "Engineering", "Upgrade")],
		"infra":   ids[sqlite.SeedKey("Product A", "Infrastructure", "")],
	}}
	// The gateway files rack work under Infrastructure only when its criteria mention racks.
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.gwCalls.Add(1)
		var req decision.Request
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		item, _ := json.Marshal(req.State.(map[string]any)["work_item"])
		crit, _ := json.Marshal(req.Questions[filing.QSection].Criteria)
		cluster, infra, upgrade := domain.SectionKey(e.sec["cluster"]), domain.SectionKey(e.sec["infra"]), domain.SectionKey(e.sec["upgrade"])
		probs := map[string]float64{cluster: 0.6, infra: 0.3, upgrade: 0.1}
		if strings.Contains(string(item), "rack") && strings.Contains(string(crit), "Racks") {
			probs = map[string]float64{infra: 0.9, cluster: 0.08, upgrade: 0.02}
		}
		out, _ := json.Marshal(map[string]any{"model": "fake", "answers": map[string]any{filing.QSection: choice(probs)}})
		w.Write(out)
	}))
	t.Cleanup(gw.Close)
	release := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.llmCalls.Add(1)
		if e.llmBlock != nil {
			select {
			case <-r.Context().Done():
			case <-e.llmBlock:
			case <-release:
			}
			return
		}
		var body struct {
			Input string `json:"input"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		out, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message",
			"content": []any{map[string]any{"type": "output_text", "text": e.llmText(body.Input)}}}}})
		w.Write(out)
	}))
	t.Cleanup(llmSrv.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.Config{SimilarItemCount: 5, DecisionModelConfidence: 0.85, DecisionModelName: "fake",
		LLMProvider: "openai", OpenAIBaseURL: llmSrv.URL, OpenAIAPIKey: "k", Location: time.UTC}
	client := decision.NewClient(gw.URL, "key", "fake")
	client.HTTPClient, client.BaseDelay, client.MaxDelay, client.MaxTries = http.DefaultClient, time.Millisecond, time.Millisecond, 2
	e.l = &Learner{DB: db, Cfg: cfg, Filer: &filing.Filer{DB: db, Cfg: cfg, Decision: client}, Notify: e.rec}
	e.llmText = func(string) string { return "[]" }
	return e
}

func (e *env) insert(t *testing.T, desc string, at time.Time) int64 {
	t.Helper()
	id, err := sqlite.InsertWorkItem(e.db, domain.WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: at, FilingState: domain.FilingPending})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) place(t *testing.T, id int64, sec string, by string) {
	t.Helper()
	conf := 0.9
	if err := sqlite.SetPlacement(e.db, id, sqlite.Placement{SectionID: e.sec[sec], PlacedBy: by, Confidence: &conf,
		ModelSectionID: e.sec["cluster"], HistoryNote: sqlite.ReasonArrival}); err != nil {
		t.Fatal(err)
	}
}

// seedDecisions files n rack and n node Work Items by a person, and corrections rack: cluster → infra.
func (e *env) seedDecisions(t *testing.T, n int) {
	t.Helper()
	now := time.Now()
	for i := 0; i < n; i++ {
		e.place(t, e.insert(t, fmt.Sprintf("rack power swap %d", i), now.Add(-time.Duration(i)*time.Hour)), "infra", domain.PlacedByPerson)
		e.place(t, e.insert(t, fmt.Sprintf("node join fix %d", i), now.Add(-time.Duration(i)*time.Hour)), "cluster", domain.PlacedByPerson)
	}
	for i := 0; i < 3; i++ {
		id := e.insert(t, fmt.Sprintf("rack cabling %d", i), now)
		e.place(t, id, "cluster", domain.PlacedByModel)
		if err := sqlite.SetPlacement(e.db, id, sqlite.Placement{SectionID: e.sec["infra"], PlacedBy: domain.PlacedByPerson, HistoryNote: sqlite.ReasonWebMove}); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) notesFor(id int64, text string) {
	e.llmText = func(string) string {
		return fmt.Sprintf(`[{"section_id":"%s","notes":%q}]`, domain.SectionKey(id), text)
	}
}

func (e *env) section(t *testing.T, key string) domain.Section {
	t.Helper()
	tax, err := sqlite.LoadTaxonomy(e.db)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := tax.Section(e.sec[key])
	return s
}

func TestRunNotesAppliesNotesThatPassTheReplayGate(t *testing.T) {
	e := newEnv(t)
	e.seedDecisions(t, 35)
	e.notesFor(e.sec["infra"], "Racks, power and cabling. Not cluster software — that is Cluster Manager.")

	rep, err := e.l.RunNotes(context.Background(), TriggerNightly)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.Gate == nil || rep.Gate.Items < MinReplayItems || rep.Gate.GainPoints() < MinGainPoints {
		t.Fatalf("notes that fix rack work must apply: %+v", rep)
	}
	infra := e.section(t, "infra")
	if !strings.HasPrefix(infra.LearnedNotes, "Racks") || infra.Description != "" {
		t.Fatalf("notes stored apart from the description: %+v", infra)
	}
	if len(rep.Candidates) != 1 || rep.Candidates[0].HistoryID == 0 {
		t.Fatalf("an applied note keeps its history row for Undo: %+v", rep.Candidates)
	}
	if len(e.rec.notes) != 1 {
		t.Fatalf("a nightly run that applied notes sends one digest, got %d", len(e.rec.notes))
	}
	var decision string
	e.db.QueryRow(`SELECT decision FROM learning_runs ORDER BY id DESC LIMIT 1`).Scan(&decision)
	if decision != sqlite.RunApplied {
		t.Fatalf("every run is logged, got %q", decision)
	}

	// No new signals: nothing to draft, no LLM call.
	calls := e.llmCalls.Load()
	rep, err = e.l.RunNotes(context.Background(), TriggerNightly)
	if err != nil || rep.Decision != sqlite.RunSkipped || e.llmCalls.Load() != calls {
		t.Fatalf("a run without new signals must skip without the LLM: %+v err=%v", rep, err)
	}
}

func TestRunNotesDiscardsNotesWithoutGainAndCachesReplay(t *testing.T) {
	e := newEnv(t)
	e.seedDecisions(t, 35)
	e.notesFor(e.sec["infra"], "Hardware in the lab.")

	rep, err := e.l.RunNotes(context.Background(), TriggerRetrospect)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Applied || rep.Decision != sqlite.RunDiscarded || len(rep.Gate.Reasons) == 0 {
		t.Fatalf("notes without gain must be discarded with reasons: %+v", rep)
	}
	if e.section(t, "infra").LearnedNotes != "" {
		t.Fatal("discarded notes must not be stored")
	}
	if len(e.rec.notes) != 0 {
		t.Fatal("no digest when nothing was applied")
	}

	// The same replay again is answered from the cache.
	before := e.gwCalls.Load()
	snap, _ := e.l.Filer.Snapshot()
	items, _ := sqlite.GetPersonDecidedItems(e.db, time.Now().AddDate(0, 0, -56))
	if _, err := e.l.replay(context.Background(), snap, items, snap.Sections()); err != nil {
		t.Fatal(err)
	}
	if e.gwCalls.Load() != before {
		t.Fatalf("identical replay requests must not be repeated (%d new calls)", e.gwCalls.Load()-before)
	}
}

func TestRunNotesSkipsWithTooFewPersonDecisions(t *testing.T) {
	e := newEnv(t)
	e.seedDecisions(t, 20)
	e.notesFor(e.sec["infra"], "Racks.")
	rep, err := e.l.RunNotes(context.Background(), TriggerNightly)
	if err != nil || rep.Decision != sqlite.RunSkipped || !strings.Contains(rep.Reason, "60") || e.llmCalls.Load() != 0 {
		t.Fatalf("fewer than 60 person decisions must skip before drafting: %+v err=%v", rep, err)
	}
}

func TestRunNotesWithoutLLMIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.l.Cfg.LLMProvider = ""
	if _, err := e.l.RunNotes(context.Background(), TriggerNightly); err != ErrNoLLM {
		t.Fatalf("want ErrNoLLM, got %v", err)
	}
}

func TestUndoRestoresPreviousNotes(t *testing.T) {
	e := newEnv(t)
	e.seedDecisions(t, 35)
	e.notesFor(e.sec["infra"], "Racks, power and cabling.")
	rep, err := e.l.RunNotes(context.Background(), TriggerNightly)
	if err != nil || !rep.Applied {
		t.Fatalf("setup: %+v %v", rep, err)
	}
	sec, err := e.l.UndoNotes(rep.Candidates[0].HistoryID, "U9")
	if err != nil || sec.ID != e.sec["infra"] || e.section(t, "infra").LearnedNotes != "" {
		t.Fatalf("undo: %+v %v", sec, err)
	}
	if _, err := e.l.UndoNotes(rep.Candidates[0].HistoryID, "U8"); err != sqlite.ErrNothingToUndo {
		t.Fatalf("a second Undo does nothing, got %v", err)
	}
}

// --- Structure Proposals ---

func (e *env) seedUnplaced(t *testing.T, n int) []int64 {
	t.Helper()
	var ids []int64
	low := 0.3
	for i := 0; i < n; i++ {
		id := e.insert(t, fmt.Sprintf("export metrics %d", i), time.Now())
		if err := sqlite.ApplyFiling(e.db, id, sqlite.FilingOutcome{Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, Confidence: &low,
			ModelSectionID: e.sec["cluster"], FilingState: domain.FilingAsking, Reason: domain.ReasonLowConfidence, HistoryNote: sqlite.ReasonArrival}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *env) category(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM taxonomy_nodes WHERE name = ? AND level = 'category'`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProposeStructureStoresAndNotifies(t *testing.T) {
	e := newEnv(t)
	ids := e.seedUnplaced(t, 5)
	eng := e.category(t, "Engineering")
	e.llmText = func(input string) string {
		if !strings.Contains(input, "export metrics 0") {
			t.Errorf("Undetermined Work Items must be in the prompt:\n%s", input)
		}
		return fmt.Sprintf(`[{"kind":"new","parent_id":%d,"name":"Telemetry","description":"Metrics export","item_ids":%s,"reason":"recurring"}]`,
			eng, idsJSON(ids))
	}
	got, err := e.l.ProposeStructure(context.Background())
	if err != nil || len(got) != 1 || got[0].Affected != 5 {
		t.Fatalf("proposals: %+v %v", got, err)
	}
	if len(e.rec.proposals) != 1 {
		t.Fatal("managers must be sent the proposals")
	}
	// Proposed again while pending: blocked, nothing new is sent.
	again, err := e.l.ProposeStructure(context.Background())
	if err != nil || len(again) != 0 || len(e.rec.proposals) != 1 {
		t.Fatalf("a pending proposal is not proposed twice: %+v %v", again, err)
	}
}

func idsJSON(ids []int64) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func TestApproveProposalAppliesOnceThroughTaxonomyEdits(t *testing.T) {
	e := newEnv(t)
	ids := e.seedUnplaced(t, 5)
	eng := e.category(t, "Engineering")
	e.llmText = func(string) string {
		return fmt.Sprintf(`[{"kind":"new","parent_id":%d,"name":"Telemetry","description":"Metrics export","item_ids":%s}]`, eng, idsJSON(ids))
	}
	woke := 0
	e.l.Wake = func() { woke++ }
	got, err := e.l.ProposeStructure(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("setup: %+v %v", got, err)
	}
	if _, err := e.l.ApproveProposal(got[0].ID, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.l.ApproveProposal(got[0].ID, "U2"); err != ErrAlreadyDecided {
		t.Fatalf("a second approval must not apply again, got %v", err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM taxonomy_nodes WHERE name = 'Telemetry' AND level = 'section' AND description = 'Metrics export'`).Scan(&n)
	if n != 1 {
		t.Fatalf("approved Section must be added once, got %d", n)
	}
	if pending, _ := sqlite.PendingReclassify(e.db); !pending || woke != 1 {
		t.Fatalf("an approved change re-files Open Work Items like a Taxonomy edit (pending=%v woke=%d)", pending, woke)
	}
}

func TestRejectedProposalCoolsDown(t *testing.T) {
	e := newEnv(t)
	ids := e.seedUnplaced(t, 5)
	eng := e.category(t, "Engineering")
	e.llmText = func(string) string {
		return fmt.Sprintf(`[{"kind":"new","parent_id":%d,"name":"Telemetry","item_ids":%s}]`, eng, idsJSON(ids))
	}
	got, _ := e.l.ProposeStructure(context.Background())
	if len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	if err := e.l.RejectProposal(got[0].ID, "U1"); err != nil {
		t.Fatal(err)
	}
	if err := e.l.RejectProposal(got[0].ID, "U2"); err != ErrAlreadyDecided {
		t.Fatalf("a proposal is decided once, got %v", err)
	}
	again, _ := e.l.ProposeStructure(context.Background())
	if len(again) != 0 {
		t.Fatalf("a rejected change is not proposed again within the cooldown: %+v", again)
	}
	e.l.Now = func() time.Time { return time.Now().AddDate(0, 0, 7*ProposalCooldownWeeks+1) }
	if again, _ := e.l.ProposeStructure(context.Background()); len(again) != 1 {
		t.Fatalf("after the cooldown it may be proposed again: %+v", again)
	}
}

// --- Second Opinion ---

// asked files an unsure Work Item whose Section Choice was made `ago` before now.
func (e *env) asked(t *testing.T, desc string, ago time.Duration) int64 {
	t.Helper()
	id := e.insert(t, desc, time.Now().Add(-ago))
	low := 0.5
	probs := fmt.Sprintf(`{"%s":0.5,"%s":0.3,"%s":0.15}`, domain.SectionKey(e.sec["cluster"]), domain.SectionKey(e.sec["infra"]), domain.SectionKey(e.sec["upgrade"]))
	if err := sqlite.ApplyFiling(e.db, id, sqlite.FilingOutcome{
		Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, Confidence: &low, ModelSectionID: e.sec["cluster"],
			FilingState: domain.FilingAsking, Reason: domain.ReasonLowConfidence, HistoryNote: sqlite.ReasonArrival},
		Judgments: []sqlite.Judgment{{Kind: "section_choice", Subject: "section", Model: "fake",
			AnswerJSON: `{"type":"choice","choice":"x","probabilities":` + probs + `,"confidence":0.5}`}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE model_judgments SET created_at = ? WHERE work_item_id = ?`, time.Now().Add(-ago), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSecondOpinionFilesUnansweredItemsAfter24Hours(t *testing.T) {
	e := newEnv(t)
	due := e.asked(t, "rack fan noise", 25*time.Hour)
	fresh := e.asked(t, "rack fan hum", time.Hour)
	var prompt string
	e.llmText = func(input string) string {
		prompt = input
		return fmt.Sprintf(`{"section_id":"%s"}`, domain.SectionKey(e.sec["infra"]))
	}
	n, err := e.l.RunDueSecondOpinions(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("one item is due: n=%d err=%v", n, err)
	}
	it, _ := sqlite.GetWorkItemByID(e.db, due)
	if it.SectionID != e.sec["infra"] || it.PlacedBy != domain.PlacedByAssistant {
		t.Fatalf("filed by the assistant: %+v", it)
	}
	if f, _ := sqlite.GetWorkItemByID(e.db, fresh); f.SectionID != 0 {
		t.Fatal("an item asked less than 24 hours ago waits for its reporter")
	}
	for _, want := range []string{domain.SectionKey(e.sec["cluster"]), domain.SectionKey(e.sec["infra"]), domain.SectionKey(e.sec["upgrade"]), "rack fan noise"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the LLM sees the model's top 3 Sections and the item; missing %q:\n%s", want, prompt)
		}
	}
	if len(e.rec.assistant) != 1 || !strings.HasSuffix(e.rec.assistant[0], ":Infrastructure") {
		t.Fatalf("the reporter is told: %v", e.rec.assistant)
	}
}

func TestSecondOpinionOutsideTopThreeIsIgnored(t *testing.T) {
	e := newEnv(t)
	id := e.asked(t, "rack fan noise", 25*time.Hour)
	e.llmText = func(string) string { return `{"section_id":"S9999"}` }
	if n, _ := e.l.RunDueSecondOpinions(context.Background()); n != 0 {
		t.Fatalf("an answer outside the top 3 files nothing, filed %d", n)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID != 0 {
		t.Fatalf("the item stays Undetermined: %+v", it)
	}
}

func TestPublishSweepStopsAtTimeout(t *testing.T) {
	e := newEnv(t)
	id := e.asked(t, "rack fan noise", time.Minute)
	e.llmBlock = make(chan struct{})
	items, _ := sqlite.GetOpenItems(e.db)
	start := time.Now()
	n := e.l.SweepUndetermined(context.Background(), items, 100*time.Millisecond)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the sweep must stop at its timeout, took %s", took)
	}
	if n != 0 {
		t.Fatalf("nothing filed when the LLM is too slow, got %d", n)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID != 0 {
		t.Fatal("the item stays Undetermined")
	}
}

func TestPublishSweepFilesStillUndeterminedItemsWithoutWaiting(t *testing.T) {
	e := newEnv(t)
	id := e.asked(t, "rack fan noise", time.Minute)
	e.llmText = func(string) string { return fmt.Sprintf(`{"section_id":"%s"}`, domain.SectionKey(e.sec["infra"])) }
	items, _ := sqlite.GetOpenItems(e.db)
	if n := e.l.SweepUndetermined(context.Background(), items, time.Minute); n != 1 {
		t.Fatalf("the publish sweep does not wait 24 hours, filed %d", n)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.PlacedBy != domain.PlacedByAssistant {
		t.Fatalf("filed by the assistant: %+v", it)
	}
}
