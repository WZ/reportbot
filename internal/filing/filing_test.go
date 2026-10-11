package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"
)

// fakeLLM serves the OpenAI Responses API; pick decides the Section key for each item ID.
type fakeLLM struct {
	srv   *httptest.Server
	mu    sync.Mutex
	pick  func(itemID int64) string
	fail  bool
	calls int
}

var workItemRe = regexp.MustCompile(`Work item (\d+)`)

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var req struct{ Input string }
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		var items []map[string]any
		for _, m := range workItemRe.FindAllStringSubmatch(req.Input, -1) {
			var id int64
			fmt.Sscan(m[1], &id)
			items = append(items, map[string]any{"id": id, "section_id": f.pick(id)})
		}
		text, _ := json.Marshal(map[string]any{"items": items})
		out, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "message",
			"content": []any{map[string]any{"type": "output_text", "text": string(text)}}}}})
		w.Write(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeGateway serves /v1/systemone; answer fills each question's answer.
type fakeGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []decision.Request
	answer   func(qid string, q decision.Question) map[string]any
	status   int
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.status != 0 {
			http.Error(w, "unavailable", g.status)
			return
		}
		var req decision.Request
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, "instructions must be text", http.StatusUnprocessableEntity)
			return
		}
		g.requests = append(g.requests, req)
		answers := map[string]any{}
		for id, q := range req.Questions {
			answers[id] = g.answer(id, q)
		}
		out, _ := json.Marshal(map[string]any{"model": "fake-1", "answers": answers, "usage": map[string]int{"input_tokens": 50, "output_tokens": 5}})
		w.Write(out)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

type recorder struct {
	mu        sync.Mutex
	filed     []Result
	overrides []string
}

func (r *recorder) Filed(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filed = append(r.filed, res)
}

func (r *recorder) PlacementOverridden(item domain.WorkItem, eventID int64, from, to domain.Section) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides = append(r.overrides, fmt.Sprintf("%d:%s->%s", item.ID, from.Label(), to.Label()))
}

type env struct {
	db      *sql.DB
	llm     *fakeLLM
	gw      *fakeGateway
	notes   *recorder
	filer   *Filer
	queue   *Queue
	section map[string]int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "filing.db"))
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
	e := &env{db: db, llm: newFakeLLM(t), gw: newFakeGateway(t), notes: &recorder{}, section: map[string]int64{
		"cluster": ids[sqlite.SeedKey("Product A", "Engineering", "Cluster Manager")],
		"upgrade": ids[sqlite.SeedKey("Product A", "Engineering", "Upgrade")],
		"infra":   ids[sqlite.SeedKey("Product A", "Infrastructure", "")],
	}}
	cfg := config.Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: e.llm.srv.URL, LLMModel: "test-llm",
		LLMExampleCount: 5, LLMExampleMaxLen: 100, LLMBatchSize: 2, DecisionModelConfidence: 0.7}
	client := decision.NewClient(e.gw.srv.URL, "key", "test-dm")
	client.HTTPClient, client.BaseDelay, client.MaxDelay, client.MaxTries = http.DefaultClient, time.Millisecond, time.Millisecond, 2
	e.filer = &Filer{DB: db, Cfg: cfg, Classify: llm.ClassifySections, Decision: client, Notify: e.notes}
	e.queue = NewQueue(e.filer)
	e.llm.pick = func(int64) string { return llm.SectionKey(e.section["cluster"]) }
	e.gw.answer = func(qid string, q decision.Question) map[string]any {
		switch q.Type {
		case "noul":
			if qid == QSectionCheck {
				return map[string]any{"type": "noul", "noul": 0.9}
			}
			return map[string]any{"type": "noul", "noul": 0.1}
		default:
			return map[string]any{"type": "choice", "choice": "in_progress", "confidence": 0.8}
		}
	}
	return e
}

// placed adds a person-placed past item for the similar-item index.
func (e *env) placed(t *testing.T, desc, author, section string, status string) int64 {
	t.Helper()
	id, err := sqlite.InsertWorkItem(e.db, domain.WorkItem{Description: desc, Author: author, Source: "slack", Status: status,
		ReportedAt: time.Now().Add(-48 * time.Hour), SectionID: e.section[section], PlacedBy: domain.PlacedByPerson, FilingState: domain.FilingFiled})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) report(t *testing.T, item domain.WorkItem) int64 {
	t.Helper()
	item.Source, item.ReportedAt = "slack", time.Now()
	ids, err := sqlite.InsertWorkItemsForFiling(e.db, []domain.WorkItem{item}, Payload{Kind: KindArrival}.JSON())
	if err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

func (e *env) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20 && e.queue.RunOne(context.Background()); i++ {
	}
}

func TestArrivalFiledWhenCheckPasses(t *testing.T) {
	e := newEnv(t)
	e.placed(t, "Shard rebalance stuck after node add", "Dev B", "cluster", domain.StatusDone)
	id := e.report(t, domain.WorkItem{Description: "Fix shard rebalance timeout", Author: "Dev A", Status: domain.StatusInProgress})
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != e.section["cluster"] || it.PlacedBy != domain.PlacedByModel || it.FilingState != domain.FilingFiled ||
		it.Confidence == nil || *it.Confidence != 0.9 {
		t.Fatalf("expected a model Placement at 0.9: %+v", it)
	}
	if len(e.notes.filed) != 1 || e.notes.filed[0].Section == nil || e.notes.filed[0].Payload.Kind != "arrival" {
		t.Fatalf("expected one Filed notification with the Section: %+v", e.notes.filed)
	}
	if len(e.gw.requests) != 1 {
		t.Fatalf("all questions go in one request, got %d", len(e.gw.requests))
	}
	req := e.gw.requests[0]
	q, ok := req.Questions[QSectionCheck]
	if !ok || q.Instructions != SectionCheckInstructions || req.Model != "test-dm" {
		t.Fatalf("unexpected check question: %+v", req)
	}
	state := req.State.(map[string]any)
	proposed := state["proposed_section"].(map[string]any)
	if proposed["name"] != "Product A › Engineering › Cluster Manager" {
		t.Fatalf("unexpected proposed_section: %v", proposed)
	}
	if sim := state["similar_past_items"].([]any); len(sim) != 1 {
		t.Fatalf("expected the similar past item in state: %v", sim)
	}
	var kinds []string
	rows, _ := e.db.Query(`SELECT kind FROM model_judgments WHERE work_item_id = ? ORDER BY id`, id)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "section_pick,section_check" {
		t.Fatalf("raw judgments must be stored: %v", kinds)
	}
}

func TestArrivalBelowThresholdAsksWithVoteChoices(t *testing.T) {
	e := newEnv(t)
	e.placed(t, "rack power supply swap", "Dev B", "infra", domain.StatusDone)
	e.placed(t, "rack power cabling", "Dev C", "infra", domain.StatusDone)
	e.placed(t, "rack power upgrade plan", "Dev C", "upgrade", domain.StatusDone)
	e.gw.answer = func(qid string, q decision.Question) map[string]any {
		return map[string]any{"type": "noul", "noul": 0.4}
	}
	id := e.report(t, domain.WorkItem{Description: "rack power failure", Author: "Dev A", Status: domain.StatusInProgress})
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.FilingState != domain.FilingAsking || it.UndeterminedReason != domain.ReasonLowConfidence || *it.Confidence != 0.4 {
		t.Fatalf("expected Undetermined pending a person's choice: %+v", it)
	}
	r := e.notes.filed[0]
	var labels []string
	for _, c := range r.Choices {
		labels = append(labels, c.Label())
	}
	if strings.Join(labels, ",") != "Cluster Manager,Infrastructure,Upgrade" {
		t.Fatalf("expected the LLM's pick then the top Sections by vote, got %v", labels)
	}
}

func TestArrivalLLMUndeterminedSkipsCheck(t *testing.T) {
	e := newEnv(t)
	e.llm.pick = func(int64) string { return llm.UndeterminedID }
	id := e.report(t, domain.WorkItem{Description: "something new", Author: "Dev A", Status: domain.StatusInProgress})
	e.drain(t)
	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.UndeterminedReason != domain.ReasonLLMUndetermined {
		t.Fatalf("expected llm_undetermined: %+v", it)
	}
	if len(e.gw.requests) != 0 {
		t.Fatalf("no decision-model request is needed: %d", len(e.gw.requests))
	}
}

func TestArrivalAsksStatusTicketAndUpdate(t *testing.T) {
	e := newEnv(t)
	existing := e.placed(t, "Shard rebalance timeout on 7.6", "Dev A", "cluster", domain.StatusInProgress)
	e.gw.answer = func(qid string, q decision.Question) map[string]any {
		switch {
		case qid == QStatus:
			return map[string]any{"type": "choice", "choice": "blocked", "confidence": 0.7}
		case qid == QTicket:
			return map[string]any{"type": "choice", "choice": "t1", "confidence": 0.7}
		case qid == UpdateQuestionID(existing):
			return map[string]any{"type": "noul", "noul": 0.83}
		default:
			return map[string]any{"type": "noul", "noul": 0.9}
		}
	}
	id := e.report(t, domain.WorkItem{Description: "Shard rebalance timeout 1234567 (dup of 7654321) seen on build 7613", Author: "Dev A",
		StatusNote: "waiting on hardware team"})
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.Status != domain.StatusBlocked || it.StatusNote != "waiting on hardware team" || it.TicketIDs != "1234567" {
		t.Fatalf("expected Status, Status Note and ticket from the model: %+v", it)
	}
	r := e.notes.filed[0]
	if r.Update == nil || r.Update.Item.ID != existing {
		t.Fatalf("expected the Update candidate: %+v", r.Update)
	}
	req := e.gw.requests[0]
	for _, q := range []string{QSectionCheck, QStatus, QTicket, UpdateQuestionID(existing)} {
		if _, ok := req.Questions[q]; !ok {
			t.Fatalf("missing question %s in the single request: %v", q, req.Questions)
		}
	}
}

func TestModelUnreachableFilesUndeterminedAfterRetries(t *testing.T) {
	e := newEnv(t)
	e.llm.fail = true
	id := e.report(t, domain.WorkItem{Description: "anything", Author: "Dev A", Status: domain.StatusDone})
	now := time.Now()
	e.filer.Now = func() time.Time { return now }
	for i := 0; i < maxAttempts; i++ {
		if !e.queue.RunOne(context.Background()) {
			t.Fatalf("attempt %d: expected a due job", i+1)
		}
		now = now.Add(retryMax + time.Minute)
	}
	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.FilingState != domain.FilingFailed || it.UndeterminedReason != domain.ReasonModelUnreachable {
		t.Fatalf("expected Undetermined after retries: %+v", it)
	}
	if len(e.notes.filed) != 1 || !e.notes.filed[0].Failed {
		t.Fatalf("expected a failure notification: %+v", e.notes.filed)
	}

	// The sweep re-queues it, and it files once the models are back.
	e.llm.fail = false
	if n := e.queue.Sweep(); n != 1 {
		t.Fatalf("expected the sweep to re-queue 1 item, got %d", n)
	}
	e.drain(t)
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID != e.section["cluster"] {
		t.Fatalf("expected the retried item filed: %+v", it)
	}
}

func TestDecisionModelUnreachableIsRetried(t *testing.T) {
	e := newEnv(t)
	e.gw.status = http.StatusServiceUnavailable
	id := e.report(t, domain.WorkItem{Description: "anything", Author: "Dev A", Status: domain.StatusDone})
	e.queue.RunOne(context.Background())
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.FilingState != domain.FilingPending {
		t.Fatalf("a failed attempt leaves the item pending for retry: %+v", it)
	}
	var state string
	e.db.QueryRow(`SELECT state FROM jobs WHERE work_item_id = ?`, id).Scan(&state)
	if state != "queued" {
		t.Fatalf("expected the job re-queued, got %s", state)
	}
}

func TestNoTaxonomyFilesUndetermined(t *testing.T) {
	db, _ := sqlite.InitDB(filepath.Join(t.TempDir(), "empty.db"))
	defer db.Close()
	notes := &recorder{}
	f := &Filer{DB: db, Cfg: config.Config{DecisionModelConfidence: 0.7}, Notify: notes,
		Classify: func(config.Config, []domain.Section, map[int64][]llm.Example, []llm.PromptItem, []llm.CorrectionLine) (llm.SectionResult, error) {
			t.Fatal("no LLM call without a Taxonomy")
			return llm.SectionResult{}, nil
		}}
	ids, _ := sqlite.InsertWorkItemsForFiling(db, []domain.WorkItem{{Description: "x", Author: "A", Source: "slack", ReportedAt: time.Now()}}, "")
	if err := f.FileItem(context.Background(), ids[0], Payload{}); err != nil {
		t.Fatal(err)
	}
	if it, _ := sqlite.GetWorkItemByID(db, ids[0]); it.UndeterminedReason != domain.ReasonNoTaxonomy {
		t.Fatalf("expected no_taxonomy: %+v", it)
	}
}

func TestReclassifyMovesPassingItemsAndNotifiesPerson(t *testing.T) {
	e := newEnv(t)
	personItem := e.placed(t, "upgrade orchestration", "Dev A", "cluster", domain.StatusInProgress)
	staysItem := e.placed(t, "rack cabling", "Dev B", "infra", domain.StatusInProgress)
	doneItem := e.placed(t, "old upgrade", "Dev B", "cluster", domain.StatusDone)
	e.gw.answer = func(string, decision.Question) map[string]any { return map[string]any{"type": "noul", "noul": 0.95} }
	// staysItem's pick is its current Section, so it is not checked or moved.
	e.llm.pick = func(id int64) string {
		if id == staysItem {
			return llm.SectionKey(e.section["infra"])
		}
		return llm.SectionKey(e.section["upgrade"])
	}

	if err := sqlite.EnqueueReclassify(e.db); err != nil {
		t.Fatal(err)
	}
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, personItem)
	if it.SectionID != e.section["upgrade"] || it.PlacedBy != domain.PlacedByModel {
		t.Fatalf("a passing check moves even person-placed items: %+v", it)
	}
	if len(e.notes.overrides) != 1 || e.notes.overrides[0] != fmt.Sprintf("%d:Cluster Manager->Upgrade", personItem) {
		t.Fatalf("the person must be told: %v", e.notes.overrides)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, staysItem); it.SectionID != e.section["infra"] {
		t.Fatalf("an unchanged pick keeps the item: %+v", it)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, doneItem); it.SectionID != e.section["cluster"] {
		t.Fatalf("done items are not re-classified: %+v", it)
	}
	if e.llm.calls != 1 {
		t.Fatalf("two open items with batch size 2 need one LLM call, got %d", e.llm.calls)
	}
}

func TestReclassifyRetiredSectionGoesUndetermined(t *testing.T) {
	e := newEnv(t)
	id := e.placed(t, "rack cabling", "Dev B", "infra", domain.StatusInProgress)
	if err := sqlite.RetireTaxonomyNode(e.db, e.section["infra"]); err != nil {
		t.Fatal(err)
	}
	e.gw.answer = func(string, decision.Question) map[string]any { return map[string]any{"type": "noul", "noul": 0.2} }
	if err := e.filer.Reclassify(context.Background()); err != nil {
		t.Fatal(err)
	}
	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.UndeterminedReason != domain.ReasonSectionRetired {
		t.Fatalf("expected section_retired: %+v", it)
	}
}

func TestRefileWithoutConfidenceKeepsCurrentSection(t *testing.T) {
	e := newEnv(t)
	id := e.placed(t, "rack cabling", "Dev A", "infra", domain.StatusInProgress)
	e.gw.answer = func(string, decision.Question) map[string]any { return map[string]any{"type": "noul", "noul": 0.3} }
	if err := sqlite.SetFilingPending(e.db, id, Payload{Kind: KindEdit}.JSON()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != e.section["infra"] || it.PlacedBy != domain.PlacedByPerson || it.FilingState != domain.FilingFiled {
		t.Fatalf("an unsure re-file keeps the person's Section: %+v", it)
	}
	r := e.notes.filed[0]
	if r.Kept == nil || r.Kept.ID != e.section["infra"] || len(r.Choices) == 0 || r.Section != nil {
		t.Fatalf("the reporter is still asked, with the kept Section: %+v", r)
	}
	if len(e.gw.requests[0].Questions) != 1 {
		t.Fatalf("an edit asks no Update question: %v", e.gw.requests[0].Questions)
	}
}

func TestFailureNotifiedOncePerWorkItem(t *testing.T) {
	e := newEnv(t)
	e.llm.fail = true
	id := e.report(t, domain.WorkItem{Description: "anything", Author: "Dev A", Status: domain.StatusDone})
	now := time.Now()
	e.filer.Now = func() time.Time { return now }
	fail := func() {
		for i := 0; i < maxAttempts; i++ {
			e.queue.RunOne(context.Background())
			now = now.Add(retryMax + time.Minute)
		}
	}
	fail()
	if e.queue.Sweep() != 1 {
		t.Fatal("expected the sweep to re-queue the failed item")
	}
	fail()
	if len(e.notes.filed) != 1 {
		t.Fatalf("a repeated failure must not notify again, got %d notifications", len(e.notes.filed))
	}
	e.llm.fail = false
	e.queue.Sweep()
	e.drain(t)
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID == 0 || len(e.notes.filed) != 2 {
		t.Fatalf("a successful retry files and notifies: %+v (%d notifications)", it, len(e.notes.filed))
	}
}

func TestRetryOfNewItemAsksUpdateQuestions(t *testing.T) {
	e := newEnv(t)
	existing := e.placed(t, "Shard rebalance timeout", "Dev A", "cluster", domain.StatusInProgress)
	e.llm.fail = true
	e.report(t, domain.WorkItem{Description: "Shard rebalance timeout fixed", Author: "Dev A", Status: domain.StatusInTesting})
	now := time.Now()
	e.filer.Now = func() time.Time { return now }
	for i := 0; i < maxAttempts; i++ {
		e.queue.RunOne(context.Background())
		now = now.Add(retryMax + time.Minute)
	}
	e.llm.fail = false
	e.queue.Sweep()
	e.drain(t)
	req := e.gw.requests[len(e.gw.requests)-1]
	if _, ok := req.Questions[UpdateQuestionID(existing)]; !ok {
		t.Fatalf("the first successful filing through a retry asks Update questions: %v", req.Questions)
	}
}
