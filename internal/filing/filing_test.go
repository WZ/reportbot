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
	"strings"
	"sync"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/storage/sqlite"
)

// fakeGateway serves /v1/systemone; answer fills each question's answer.
type fakeGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []decision.Request
	answer   func(qid string, q decision.Question) map[string]any
	status   int
	// section answers the Section Choice: probabilities by Section key.
	section func(req decision.Request) map[string]float64
}

// choiceAnswer builds a Choice answer whose confidence is the top probability.
func choiceAnswer(probs map[string]float64) map[string]any {
	best, bestP := "", -1.0
	for k, p := range probs {
		if p > bestP || (p == bestP && k < best) {
			best, bestP = k, p
		}
	}
	return map[string]any{"type": "choice", "choice": best, "probabilities": probs, "confidence": bestP}
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
			if id == QSection {
				answers[id] = choiceAnswer(g.section(req))
				continue
			}
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
	fallbacks []string
}

func (r *recorder) FiledByFallback(item domain.WorkItem, s domain.Section, atPublish bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fallbacks = append(r.fallbacks, fmt.Sprintf("%d:%s:%t", item.ID, s.Label(), atPublish))
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
	e := &env{db: db, gw: newFakeGateway(t), notes: &recorder{}, section: map[string]int64{
		"cluster": ids[sqlite.SeedKey("Product A", "Engineering", "Cluster Manager")],
		"upgrade": ids[sqlite.SeedKey("Product A", "Engineering", "Upgrade")],
		"infra":   ids[sqlite.SeedKey("Product A", "Infrastructure", "")],
	}}
	cfg := config.Config{SimilarItemCount: 5, DecisionModelConfidence: 0.7}
	client := decision.NewClient(e.gw.srv.URL, "key", "test-dm")
	client.HTTPClient, client.BaseDelay, client.MaxDelay, client.MaxTries = http.DefaultClient, time.Millisecond, time.Millisecond, 2
	e.filer = &Filer{DB: db, Cfg: cfg, Decision: client, Notify: e.notes}
	e.queue = NewQueue(e.filer)
	e.pick("cluster", 0.9)
	e.gw.answer = func(qid string, q decision.Question) map[string]any {
		if q.Type == "noul" {
			return map[string]any{"type": "noul", "noul": 0.1}
		}
		return map[string]any{"type": "choice", "choice": "in_progress", "confidence": 0.8}
	}
	return e
}

// pick makes the decision model choose section with probability p; the rest is spread
// so infra comes second and upgrade third.
func (e *env) pick(section string, p float64) {
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	e.gw.section = func(decision.Request) map[string]float64 {
		probs := map[string]float64{}
		rest := (1 - p)
		for _, name := range []string{"infra", "upgrade", "cluster"} {
			if name != section {
				probs[domain.SectionKey(e.section[name])] = rest * 0.6
				rest *= 0.4
			}
		}
		probs[domain.SectionKey(e.section[section])] = p
		return probs
	}
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

func TestArrivalFiledWhenConfident(t *testing.T) {
	e := newEnv(t)
	e.placed(t, "Shard rebalance stuck after node add", "Dev B", "cluster", domain.StatusDone)
	e.placed(t, "Shard rebalance stuck after node add", "Dev B", "cluster", domain.StatusDone)
	id := e.report(t, domain.WorkItem{Description: "Fix shard rebalance timeout", Author: "Dev A", Status: domain.StatusInProgress})
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != e.section["cluster"] || it.PlacedBy != domain.PlacedByModel || it.FilingState != domain.FilingFiled ||
		it.Confidence == nil || *it.Confidence != 0.9 {
		t.Fatalf("expected a model Placement at 0.9: %+v", it)
	}
	if len(e.notes.filed) != 1 || e.notes.filed[0].Section == nil || e.notes.filed[0].Payload.Kind != KindArrival {
		t.Fatalf("expected one Filed notification with the Section: %+v", e.notes.filed)
	}
	if len(e.gw.requests) != 1 {
		t.Fatalf("one request per Work Item, got %d", len(e.gw.requests))
	}
	req := e.gw.requests[0]
	q, ok := req.Questions[QSection]
	if !ok || q.Type != "choice" || q.Instructions != SectionInstructions || req.Model != "test-dm" {
		t.Fatalf("unexpected Section question: %+v", req)
	}
	crit := q.Criteria.(map[string]any)
	if len(crit) != 3 || crit[domain.SectionKey(e.section["cluster"])] != "Product A › Engineering › Cluster Manager" {
		t.Fatalf("criteria are the active Sections by stable key: %v", crit)
	}
	sim := req.State.(map[string]any)["similar_past_items"].([]any)
	if len(sim) != 1 {
		t.Fatalf("similar items are de-duplicated by text: %v", sim)
	}
	var kinds []string
	rows, _ := e.db.Query(`SELECT kind FROM model_judgments WHERE work_item_id = ? ORDER BY id`, id)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		kinds = append(kinds, k)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "section_choice" {
		t.Fatalf("the raw Choice answer must be stored: %v", kinds)
	}
}

func TestArrivalBelowThresholdAsksWithTopSections(t *testing.T) {
	e := newEnv(t)
	e.pick("upgrade", 0.4)
	id := e.report(t, domain.WorkItem{Description: "rack power failure", Author: "Dev A", Status: domain.StatusInProgress})
	e.drain(t)

	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.FilingState != domain.FilingAsking || it.UndeterminedReason != domain.ReasonLowConfidence || *it.Confidence != 0.4 {
		t.Fatalf("expected Undetermined pending a person's choice: %+v", it)
	}
	var labels []string
	for _, c := range e.notes.filed[0].Choices {
		labels = append(labels, c.Label())
	}
	if strings.Join(labels, ",") != "Upgrade,Infrastructure,Cluster Manager" {
		t.Fatalf("expected the decision model's top 3 Sections by probability, got %v", labels)
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
			return map[string]any{"type": "noul", "noul": 0.1}
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
	for _, q := range []string{QSection, QStatus, QTicket, UpdateQuestionID(existing)} {
		if _, ok := req.Questions[q]; !ok {
			t.Fatalf("missing question %s in the single request: %v", q, req.Questions)
		}
	}
}

func TestDecisionModelUnreachableFilesUndeterminedAfterRetries(t *testing.T) {
	e := newEnv(t)
	e.gw.status = http.StatusServiceUnavailable
	id := e.report(t, domain.WorkItem{Description: "anything", Author: "Dev A", Status: domain.StatusDone})
	now := time.Now()
	e.filer.Now = func() time.Time { return now }
	e.queue.RunOne(context.Background())
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.FilingState != domain.FilingPending {
		t.Fatalf("a failed attempt leaves the item pending for retry: %+v", it)
	}
	for i := 1; i < maxAttempts; i++ {
		now = now.Add(retryMax + time.Minute)
		if !e.queue.RunOne(context.Background()) {
			t.Fatalf("attempt %d: expected a due job", i+1)
		}
	}
	it, _ := sqlite.GetWorkItemByID(e.db, id)
	if it.SectionID != 0 || it.FilingState != domain.FilingFailed || it.UndeterminedReason != domain.ReasonModelUnreachable {
		t.Fatalf("expected Undetermined after retries: %+v", it)
	}
	if len(e.notes.filed) != 1 || !e.notes.filed[0].Failed {
		t.Fatalf("expected a failure notification: %+v", e.notes.filed)
	}

	// The sweep re-queues it, and it files once the model is back.
	e.gw.status = 0
	if n := e.queue.Sweep(); n != 1 {
		t.Fatalf("expected the sweep to re-queue 1 item, got %d", n)
	}
	e.drain(t)
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID != e.section["cluster"] {
		t.Fatalf("expected the retried item filed: %+v", it)
	}
}

func TestNoTaxonomyFilesUndetermined(t *testing.T) {
	db, _ := sqlite.InitDB(filepath.Join(t.TempDir(), "empty.db"))
	defer db.Close()
	notes := &recorder{}
	f := &Filer{DB: db, Cfg: config.Config{DecisionModelConfidence: 0.7}, Notify: notes}
	ids, _ := sqlite.InsertWorkItemsForFiling(db, []domain.WorkItem{{Description: "x", Author: "A", Source: "slack", ReportedAt: time.Now()}}, "")
	if err := f.FileItem(context.Background(), ids[0], Payload{}); err != nil {
		t.Fatal(err)
	}
	if it, _ := sqlite.GetWorkItemByID(db, ids[0]); it.UndeterminedReason != domain.ReasonNoTaxonomy {
		t.Fatalf("expected no_taxonomy: %+v", it)
	}
}

func TestReclassifyMovesConfidentItemsAndNotifiesPerson(t *testing.T) {
	e := newEnv(t)
	personItem := e.placed(t, "upgrade orchestration", "Dev A", "cluster", domain.StatusInProgress)
	staysItem := e.placed(t, "rack cabling", "Dev B", "infra", domain.StatusInProgress)
	unsureItem := e.placed(t, "monitoring", "Dev B", "cluster", domain.StatusInProgress)
	doneItem := e.placed(t, "old upgrade", "Dev B", "cluster", domain.StatusDone)
	e.gw.mu.Lock()
	e.gw.section = func(req decision.Request) map[string]float64 {
		desc := req.State.(map[string]any)["work_item"].(map[string]any)["description"].(string)
		switch desc {
		case "rack cabling":
			return map[string]float64{domain.SectionKey(e.section["infra"]): 0.95, domain.SectionKey(e.section["upgrade"]): 0.05}
		case "monitoring":
			return map[string]float64{domain.SectionKey(e.section["upgrade"]): 0.5, domain.SectionKey(e.section["cluster"]): 0.5}
		}
		return map[string]float64{domain.SectionKey(e.section["upgrade"]): 0.95, domain.SectionKey(e.section["cluster"]): 0.05}
	}
	e.gw.mu.Unlock()

	if err := sqlite.EnqueueReclassify(e.db); err != nil {
		t.Fatal(err)
	}
	e.drain(t)

	if it, _ := sqlite.GetWorkItemByID(e.db, personItem); it.SectionID != e.section["upgrade"] || it.PlacedBy != domain.PlacedByModel {
		t.Fatalf("a confident pick moves even person-placed items: %+v", it)
	}
	if len(e.notes.overrides) != 1 || e.notes.overrides[0] != fmt.Sprintf("%d:Cluster Manager->Upgrade", personItem) {
		t.Fatalf("the person must be told: %v", e.notes.overrides)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, staysItem); it.SectionID != e.section["infra"] {
		t.Fatalf("an unchanged pick keeps the item: %+v", it)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, unsureItem); it.SectionID != e.section["cluster"] {
		t.Fatalf("an unsure pick keeps the item: %+v", it)
	}
	if it, _ := sqlite.GetWorkItemByID(e.db, doneItem); it.SectionID != e.section["cluster"] {
		t.Fatalf("done items are not re-filed: %+v", it)
	}
	if len(e.gw.requests) != 3 {
		t.Fatalf("one decision-model request per Open Work Item, got %d", len(e.gw.requests))
	}
}

func TestReclassifyRetiredSectionGoesUndetermined(t *testing.T) {
	e := newEnv(t)
	id := e.placed(t, "rack cabling", "Dev B", "infra", domain.StatusInProgress)
	if err := sqlite.RetireTaxonomyNode(e.db, e.section["infra"]); err != nil {
		t.Fatal(err)
	}
	e.pick("cluster", 0.3)
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
	e.pick("cluster", 0.3)
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
	e.gw.status = http.StatusServiceUnavailable
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
	e.gw.status = 0
	e.queue.Sweep()
	e.drain(t)
	if it, _ := sqlite.GetWorkItemByID(e.db, id); it.SectionID == 0 || len(e.notes.filed) != 2 {
		t.Fatalf("a successful retry files and notifies: %+v (%d notifications)", it, len(e.notes.filed))
	}
}

func TestRetryOfNewItemAsksUpdateQuestions(t *testing.T) {
	e := newEnv(t)
	existing := e.placed(t, "Shard rebalance timeout", "Dev A", "cluster", domain.StatusInProgress)
	e.gw.status = http.StatusServiceUnavailable
	e.report(t, domain.WorkItem{Description: "Shard rebalance timeout fixed", Author: "Dev A", Status: domain.StatusInTesting})
	now := time.Now()
	e.filer.Now = func() time.Time { return now }
	for i := 0; i < maxAttempts; i++ {
		e.queue.RunOne(context.Background())
		now = now.Add(retryMax + time.Minute)
	}
	e.gw.status = 0
	e.queue.Sweep()
	e.drain(t)
	req := e.gw.requests[len(e.gw.requests)-1]
	if _, ok := req.Questions[UpdateQuestionID(existing)]; !ok {
		t.Fatalf("the first successful filing through a retry asks Update questions: %v", req.Questions)
	}
}

func TestRetryOfFiledItemAsksNoUpdateQuestions(t *testing.T) {
	e := newEnv(t)
	e.placed(t, "Shard rebalance timeout", "Dev A", "cluster", domain.StatusInProgress)
	id := e.report(t, domain.WorkItem{Description: "Shard rebalance timeout fixed", Author: "Dev A", Status: domain.StatusInTesting})
	e.drain(t)
	if err := sqlite.SetFilingPending(e.db, id, Payload{Kind: KindRetry}.JSON()); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if n := len(e.gw.requests[len(e.gw.requests)-1].Questions); n != 1 {
		t.Fatalf("a retry of an already-filed Work Item asks only the Section, got %d questions", n)
	}
}
