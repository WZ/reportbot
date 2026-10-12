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
	"reportbot/internal/storage/sqlite"
)

type recorder struct {
	mu        sync.Mutex
	proposals [][]StoredProposal
}

func (r *recorder) ProposalsReady(ps []StoredProposal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.proposals = append(r.proposals, ps)
}

type env struct {
	db       *sql.DB
	l        *Learner
	rec      *recorder
	sec      map[string]int64
	llmCalls atomic.Int32
	llmText  func(input string) string
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
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.llmCalls.Add(1)
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
	cfg := config.Config{LLMProvider: "openai", OpenAIBaseURL: llmSrv.URL, OpenAIAPIKey: "k", Location: time.UTC}
	e.l = &Learner{DB: db, Cfg: cfg, Notify: e.rec}
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

func TestProposeStructureWithoutLLMIsDisabled(t *testing.T) {
	e := newEnv(t)
	e.l.Cfg.LLMProvider = ""
	if _, err := e.l.ProposeStructure(context.Background()); err != ErrNoLLM {
		t.Fatalf("want ErrNoLLM, got %v", err)
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

func TestProposeStructureDropsInvalidProposals(t *testing.T) {
	e := newEnv(t)
	ids := e.seedUnplaced(t, 5)
	eng := e.category(t, "Engineering")
	e.llmText = func(string) string {
		return fmt.Sprintf(`[{"kind":"new","parent_id":%d,"name":"<!channel> Telemetry","description":"d","item_ids":%s}]`, eng, idsJSON(ids))
	}
	got, err := e.l.ProposeStructure(context.Background())
	if err != nil || len(got) != 0 || len(e.rec.proposals) != 0 {
		t.Fatalf("an invalid proposal is dropped, not stored or sent: %+v %v", got, err)
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM structure_proposals`).Scan(&n)
	if n != 0 {
		t.Fatal("nothing stored")
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
