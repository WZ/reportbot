package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web"
)

const testWeek = "2026-03-16"

type user struct{ id, name string }

var (
	manager = user{"UMGR", "Manager"}
	alice   = user{"UALICE", "alice"}
	bob     = user{"UBOB", "bob"}
	carol   = user{"UCAROL", "carol"}
)

type testEnv struct {
	t       *testing.T
	db      *sql.DB
	cfg     web.Config
	handler http.Handler
	woke    atomic.Int32
	sec     map[string]int64 // seeded node ids by name
	items   map[string]int64 // seeded Work Item ids by key
}

func conf(f float64) *float64 { return &f }

// newEnv builds a server over a fresh database: Taxonomy Platform › Storage › {Ingest, Query}
// and Platform › Ops (a Category that is itself a Section), plus three Work Items.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ids, err := sqlite.SeedTaxonomy(db, []sqlite.SeedArea{{
		Name: "Platform",
		Categories: []sqlite.SeedCategory{
			{Name: "Storage", Sections: []string{"Ingest", "Query"}},
			{Name: "Ops"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{
		t:  t,
		db: db,
		cfg: web.Config{
			TeamName:                "TestTeam",
			ManagerSlackIDs:         []string{manager.id},
			WebSessionSecret:        "test-secret-32-bytes-long-xxxxx",
			ReportOutputDir:         t.TempDir(),
			DecisionModelConfidence: 0.7,
			Location:                time.UTC,
		},
		sec: map[string]int64{
			"Ingest":  ids[sqlite.SeedKey("Platform", "Storage", "Ingest")],
			"Query":   ids[sqlite.SeedKey("Platform", "Storage", "Query")],
			"Storage": ids[sqlite.SeedKey("Platform", "Storage", "")],
			"Ops":     ids[sqlite.SeedKey("Platform", "Ops", "")],
		},
		items: map[string]int64{},
	}
	reported := time.Date(2026, 3, 17, 10, 0, 0, 0, time.UTC)
	for key, it := range map[string]domain.WorkItem{
		"alice": {Description: "Fix ingest lag", Author: "Alice", AuthorID: alice.id, Source: "slack", Status: domain.StatusInProgress,
			SectionID: e.sec["Ingest"], PlacedBy: domain.PlacedByModel, Confidence: conf(0.95)},
		"bob": {Description: "Mystery task", Author: "Bob", AuthorID: bob.id, Source: "gitlab", Status: domain.StatusInProgress,
			UndeterminedReason: domain.ReasonLowConfidence},
		"carol": {Description: "Tune query cache", Author: "Carol", Source: "slack", Status: domain.StatusInTesting,
			SectionID: e.sec["Query"], PlacedBy: domain.PlacedByModel, Confidence: conf(0.4)},
	} {
		it.ReportedAt = reported
		it.FilingState = domain.FilingFiled
		id, err := sqlite.InsertWorkItem(db, it)
		if err != nil {
			t.Fatal(err)
		}
		e.items[key] = id
	}
	e.handler = NewServer(e.cfg, db, func() { e.woke.Add(1) }, web.PublishHooks{}).Handler
	return e
}

// do sends a signed-in request; POSTs carry the form and a matching CSRF cookie and header.
func (e *testEnv) do(method, path string, u user, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	var req *http.Request
	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", "csrf-test")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.AddCookie(&http.Cookie{Name: "_csrf", Value: "csrf-test"})
	cookie, err := web.CreateSessionCookie(e.cfg.WebSessionSecret,
		web.SessionPayload{UserID: u.id, UserName: u.name, ExpiresAt: time.Now().Add(time.Hour)}, false)
	if err != nil {
		e.t.Fatal(err)
	}
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, req)
	return rr
}

func (e *testEnv) item(key string) domain.WorkItem {
	e.t.Helper()
	it, err := sqlite.GetWorkItemByID(e.db, e.items[key])
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *testEnv) node(id int64) domain.TaxonomyNode {
	e.t.Helper()
	tax, err := sqlite.LoadTaxonomy(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	n, ok := tax.Node(id)
	if !ok {
		e.t.Fatalf("node %d not found", id)
	}
	return n
}

func expectCode(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("expected %d, got %d: %s", want, rr.Code, rr.Body.String())
	}
}

func itemPath(id int64, suffix string) string { return fmt.Sprintf("/items/%d%s", id, suffix) }

func TestLivePage_RequiresSignIn(t *testing.T) {
	e := newEnv(t)
	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	expectCode(t, rr, http.StatusFound)
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestLivePage_UndeterminedFirstWithSectionPaths(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodGet, "/?week="+testWeek, bob, nil)
	expectCode(t, rr, http.StatusOK)
	body := rr.Body.String()

	und, mystery, ingest := strings.Index(body, ">Undetermined<"), strings.Index(body, "Mystery task"), strings.Index(body, "Fix ingest lag")
	if und < 0 || mystery < 0 || ingest < 0 {
		t.Fatalf("expected Undetermined block and both Work Items, got: %s", body)
	}
	if !(und < mystery && mystery < ingest) {
		t.Errorf("expected Undetermined first: und=%d mystery=%d ingest=%d", und, mystery, ingest)
	}
	for _, want := range []string{"Platform › Storage › Ingest", "Platform › Storage › Query", "placed by model · 95%", "Undetermined: low_confidence", "low confidence", `hx-trigger="every 15s`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected page to contain %q", want)
		}
	}
	if !strings.Contains(body, `hx-get="`+itemPath(e.items["bob"], "/edit")+`"`) {
		t.Error("expected an edit button on the viewer's own Work Item")
	}
	if strings.Contains(body, `hx-get="`+itemPath(e.items["alice"], "/edit")+`"`) {
		t.Error("expected no edit button on another author's Work Item")
	}
}

func TestLivePage_PendingItemNeedsAttention(t *testing.T) {
	e := newEnv(t)
	if err := sqlite.SetFilingPending(e.db, e.items["alice"], "{}"); err != nil {
		t.Fatal(err)
	}
	body := e.do(http.MethodGet, "/?week="+testWeek, alice, nil).Body.String()
	if !strings.Contains(body, "filing…") {
		t.Error("expected a pending Work Item to show filing…")
	}
}

func TestItemRoutes_Permissions(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"description": {"Fix ingest lag"}, "status": {"done"}}

	expectCode(t, e.do(http.MethodGet, itemPath(e.items["alice"], "/edit"), bob, nil), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), bob, form), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/move"), bob, url.Values{"section_id": {"0"}}), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/delete"), bob, nil), http.StatusForbidden)
	if e.item("alice").Status != domain.StatusInProgress {
		t.Fatal("a forbidden edit changed the Work Item")
	}

	rr := e.do(http.MethodGet, itemPath(e.items["alice"], "/edit"), alice, nil)
	expectCode(t, rr, http.StatusOK)
	if !strings.Contains(rr.Body.String(), `<option value="blocked"`) {
		t.Error("expected the edit form to offer every Status")
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, form), http.StatusOK)
	if e.item("alice").Status != domain.StatusDone {
		t.Error("expected the owner's edit to be saved")
	}

	// No author ID: a display name never grants edits, even one that matches the author.
	carolForm := url.Values{"description": {"Tune query cache"}, "status": {"blocked"}, "status_note": {"waiting on infra"}}
	expectCode(t, e.do(http.MethodGet, itemPath(e.items["carol"], "/edit"), carol, nil), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["carol"], ""), carol, carolForm), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["carol"], "/move"), carol, url.Values{"section_id": {"0"}}), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["carol"], "/delete"), carol, nil), http.StatusForbidden)
	if it := e.item("carol"); it.Status != domain.StatusInTesting || it.SectionID != e.sec["Query"] {
		t.Fatalf("a name match must not change the item: %+v", it)
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["carol"], ""), manager, carolForm), http.StatusOK)
	if it := e.item("carol"); it.Status != domain.StatusBlocked || it.StatusNote != "waiting on infra" {
		t.Errorf("expected the manager's edit saved, got %+v", it)
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["carol"], "/move"), manager, url.Values{"section_id": {"0"}}), http.StatusOK)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/move"), alice, url.Values{"section_id": {"0"}}), http.StatusOK)

	managerForm := url.Values{"description": {"Mystery task"}, "status": {"in testing"}}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["bob"], ""), manager, managerForm), http.StatusOK)
	if e.item("bob").Status != domain.StatusInTesting {
		t.Error("expected a manager to edit any Work Item")
	}
}

func TestUpdateItem_RejectsBadInput(t *testing.T) {
	e := newEnv(t)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, url.Values{"description": {""}, "status": {"done"}}), http.StatusBadRequest)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, url.Values{"description": {"x"}, "status": {"in review"}}), http.StatusBadRequest)
}

func TestUpdateItem_TextChangeRequeuesFiling(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"description": {"Fix ingest lag on shard 3"}, "status": {"in progress"}, "section_id": {fmt.Sprint(e.sec["Ingest"])}}
	rr := e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, form)
	expectCode(t, rr, http.StatusOK)

	if it := e.item("alice"); it.FilingState != domain.FilingPending || it.Description != "Fix ingest lag on shard 3" {
		t.Errorf("expected new text and pending filing, got %+v", it)
	}
	var payload string
	if err := e.db.QueryRow(`SELECT payload FROM jobs WHERE kind = ? AND work_item_id = ? AND state = 'queued'`, sqlite.JobFile, e.items["alice"]).Scan(&payload); err != nil {
		t.Fatalf("expected a queued filing job: %v", err)
	}
	if !strings.Contains(payload, `"kind":"edit"`) {
		t.Errorf("expected an edit payload, got %s", payload)
	}
	if e.woke.Load() != 1 {
		t.Errorf("expected wake once, got %d", e.woke.Load())
	}
	if rr.Header().Get("HX-Trigger-After-Settle") != "report-changed" {
		t.Error("expected the response to refresh the report")
	}
}

func TestUpdateItem_SectionChangeIsPersonPlacement(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"description": {"Fix ingest lag in query"}, "status": {"in progress"}, "section_id": {fmt.Sprint(e.sec["Query"])}}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], ""), alice, form), http.StatusOK)
	it := e.item("alice")
	if it.SectionID != e.sec["Query"] || it.PlacedBy != domain.PlacedByPerson || it.FilingState != domain.FilingFiled {
		t.Errorf("expected a person Placement in Query, got %+v", it)
	}
	if e.woke.Load() != 0 {
		t.Error("expected no re-filing when a person picked the Section")
	}
}

func TestMoveItem_RecordsPersonPlacement(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodPost, itemPath(e.items["bob"], "/move"), bob, url.Values{"section_id": {fmt.Sprint(e.sec["Query"])}})
	expectCode(t, rr, http.StatusOK)
	if !strings.Contains(rr.Body.String(), "placed by person") {
		t.Errorf("expected the row to show the new Placement, got %s", rr.Body.String())
	}
	if it := e.item("bob"); it.SectionID != e.sec["Query"] || it.PlacedBy != domain.PlacedByPerson {
		t.Errorf("expected bob's Work Item in Query placed by person, got %+v", it)
	}
	var reason, actor string
	if err := e.db.QueryRow(`SELECT reason, actor_id FROM placement_history WHERE work_item_id = ? ORDER BY id DESC LIMIT 1`, e.items["bob"]).Scan(&reason, &actor); err != nil {
		t.Fatal(err)
	}
	if reason != sqlite.ReasonWebMove || actor != bob.id {
		t.Errorf("expected history reason=%s actor=%s, got %s %s", sqlite.ReasonWebMove, bob.id, reason, actor)
	}

	expectCode(t, e.do(http.MethodPost, itemPath(e.items["bob"], "/move"), bob, url.Values{"section_id": {"0"}}), http.StatusOK)
	if e.item("bob").SectionID != 0 {
		t.Error("expected section_id=0 to move the Work Item to Undetermined")
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["bob"], "/move"), bob, url.Values{"section_id": {fmt.Sprint(e.sec["Storage"])}}), http.StatusBadRequest)
}

func TestDeleteItem(t *testing.T) {
	e := newEnv(t)
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/delete"), alice, nil), http.StatusOK)
	if _, err := sqlite.GetWorkItemByID(e.db, e.items["alice"]); err != sql.ErrNoRows {
		t.Errorf("expected the Work Item deleted, got err=%v", err)
	}
	expectCode(t, e.do(http.MethodPost, itemPath(e.items["alice"], "/delete"), alice, nil), http.StatusNotFound)
}

func TestPreview_TeamAndBoss(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodGet, "/preview?mode=team&week="+testWeek, bob, nil)
	expectCode(t, rr, http.StatusOK)
	team := rr.Body.String()
	if !strings.Contains(team, "<strong>Alice</strong> - Fix ingest lag (in progress)") || !strings.Contains(team, "<h4>Undetermined</h4>") {
		t.Errorf("unexpected team preview: %s", team)
	}

	boss := e.do(http.MethodGet, "/preview?mode=boss&week="+testWeek, bob, nil).Body.String()
	if strings.Contains(boss, "<strong>Alice</strong> -") || !strings.Contains(boss, "Fix ingest lag (in progress)") {
		t.Errorf("unexpected boss preview: %s", boss)
	}
	if !strings.Contains(boss, "Alice") {
		t.Error("expected the boss preview to name authors in the Category heading")
	}
}

func TestGenerate_WarnsThenPublishes(t *testing.T) {
	e := newEnv(t)
	expectCode(t, e.do(http.MethodPost, "/generate?week="+testWeek, bob, nil), http.StatusForbidden)

	rr := e.do(http.MethodPost, "/generate?week="+testWeek, manager, nil)
	expectCode(t, rr, http.StatusOK)
	if body := rr.Body.String(); !strings.Contains(body, "Undetermined") || !strings.Contains(body, "confirm=1") {
		t.Fatalf("expected an Undetermined warning with a confirm button, got %s", body)
	}
	if _, ok, _ := sqlite.GetPublishedReport(e.db, testWeek); ok {
		t.Fatal("expected nothing published before confirmation")
	}

	rr = e.do(http.MethodPost, "/generate?week="+testWeek+"&confirm=1", manager, nil)
	expectCode(t, rr, http.StatusOK)
	m := regexp.MustCompile(`/generate/([^/]+)/status`).FindStringSubmatch(rr.Body.String())
	if m == nil {
		t.Fatalf("expected a polling element, got %s", rr.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	var status string
	for {
		status = e.do(http.MethodGet, "/generate/"+m[1]+"/status", manager, nil).Body.String()
		if !strings.Contains(status, "spinner") || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(status, "flash-success") {
		t.Fatalf("expected publish to succeed, got %s", status)
	}

	rep, ok, err := sqlite.GetPublishedReport(e.db, testWeek)
	if err != nil || !ok {
		t.Fatalf("expected a Published Report row: ok=%v err=%v", ok, err)
	}
	if rep.PublishedBy != manager.id || rep.UndeterminedCount != 1 || !strings.Contains(rep.TeamMarkdown, "Fix ingest lag") {
		t.Errorf("unexpected Published Report: %+v", rep)
	}
	for _, pattern := range []string{"*.md", "*.eml"} {
		files, _ := filepath.Glob(filepath.Join(e.cfg.ReportOutputDir, pattern))
		if len(files) != 1 {
			t.Errorf("expected one %s file, got %v", pattern, files)
			continue
		}
		if fi, err := os.Stat(files[0]); err != nil || fi.Size() == 0 {
			t.Errorf("expected %s to be written", files[0])
		}
	}
}

func TestReportBody_NoContentUntilVersionChanges(t *testing.T) {
	e := newEnv(t)
	version, err := sqlite.ReportVersion(e.db)
	if err != nil {
		t.Fatal(err)
	}
	path := "/report/body?week=" + testWeek + "&v=" + version
	rr := e.do(http.MethodGet, path, bob, nil)
	expectCode(t, rr, http.StatusNoContent)
	if rr.Body.Len() != 0 {
		t.Error("expected an empty 204 body")
	}

	if err := sqlite.UpdateWorkItemFields(e.db, e.items["alice"], "Fix ingest lag", domain.StatusBlocked, "", ""); err != nil {
		t.Fatal(err)
	}
	rr = e.do(http.MethodGet, path, bob, nil)
	expectCode(t, rr, http.StatusOK)
	if body := rr.Body.String(); !strings.Contains(body, `id="report-body"`) || strings.Contains(body, "&v="+version+`"`) {
		t.Errorf("expected a fresh body with the new version, got %s", body)
	}
}

func TestPublishedPages(t *testing.T) {
	e := newEnv(t)
	if err := sqlite.SavePublishedReport(e.db, sqlite.PublishedReport{
		WeekStart: testWeek, TeamMarkdown: "### Platform\n\n- **Alice** - team line (done)\n",
		BossMarkdown: "### Platform\n\n- boss line (done)\n", PublishedBy: manager.id,
	}); err != nil {
		t.Fatal(err)
	}
	rr := e.do(http.MethodGet, "/published", bob, nil)
	expectCode(t, rr, http.StatusOK)
	if !strings.Contains(rr.Body.String(), `href="/published/`+testWeek+`"`) {
		t.Errorf("expected the week listed, got %s", rr.Body.String())
	}

	team := e.do(http.MethodGet, "/published/"+testWeek, bob, nil).Body.String()
	if !strings.Contains(team, "<strong>Alice</strong> - team line (done)") || strings.Contains(team, "boss line") {
		t.Errorf("expected the team version, got %s", team)
	}
	boss := e.do(http.MethodGet, "/published/"+testWeek+"?mode=boss", bob, nil).Body.String()
	if !strings.Contains(boss, "boss line (done)") {
		t.Errorf("expected the boss version, got %s", boss)
	}
	expectCode(t, e.do(http.MethodGet, "/published/2020-01-06", bob, nil), http.StatusNotFound)
}

// --- Taxonomy ---

// clearReclassify marks queued re-classification done so the next edit must queue a new one.
func (e *testEnv) clearReclassify() {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE jobs SET state = 'done' WHERE kind = ?`, sqlite.JobReclassify); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) expectReclassifyQueued(step string) {
	e.t.Helper()
	pending, err := sqlite.PendingReclassify(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	if !pending {
		e.t.Errorf("%s: expected re-classification queued", step)
	}
}

func TestTaxonomyPage(t *testing.T) {
	e := newEnv(t)
	expectCode(t, e.do(http.MethodGet, "/taxonomy", bob, nil), http.StatusForbidden)

	rr := e.do(http.MethodGet, "/taxonomy", manager, nil)
	expectCode(t, rr, http.StatusOK)
	body := rr.Body.String()
	for _, want := range []string{"Platform", "Storage", "Ingest", "Ops", `name="csrf_token"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected taxonomy page to contain %q", want)
		}
	}
	if strings.Contains(body, "Re-filing") {
		t.Error("expected no re-classifying note while nothing is queued")
	}
	if err := sqlite.EnqueueReclassify(e.db); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.do(http.MethodGet, "/taxonomy", manager, nil).Body.String(), "Re-filing Open Work Items") {
		t.Error("expected the re-classifying note while a job is queued")
	}
}

func TestTaxonomyEdits_QueueReclassification(t *testing.T) {
	e := newEnv(t)
	storage := fmt.Sprint(e.sec["Storage"])
	steps := []struct {
		name string
		path string
		form url.Values
	}{
		{"add", "/taxonomy/nodes", url.Values{"parent_id": {storage}, "level": {"section"}, "name": {"Retention"}, "description": {"Data retention"}}},
		{"rename", fmt.Sprintf("/taxonomy/nodes/%d", e.sec["Query"]), url.Values{"name": {"Search"}, "description": {"Query and search"}}},
		{"move", fmt.Sprintf("/taxonomy/nodes/%d/move", e.sec["Query"]), url.Values{"dir": {"up"}}},
		{"retire", fmt.Sprintf("/taxonomy/nodes/%d/retire", e.sec["Ops"]), nil},
	}
	for i, s := range steps {
		e.clearReclassify()
		rr := e.do(http.MethodPost, s.path, manager, s.form)
		expectCode(t, rr, http.StatusSeeOther)
		e.expectReclassifyQueued(s.name)
		if got := e.woke.Load(); got != int32(i+1) {
			t.Errorf("%s: expected wake count %d, got %d", s.name, i+1, got)
		}
	}

	tax, err := sqlite.LoadTaxonomy(e.db)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range tax.Children(e.sec["Storage"]) {
		names = append(names, n.Name)
	}
	if got := strings.Join(names, ","); got != "Search,Ingest,Retention" {
		t.Errorf("expected Storage sections Search,Ingest,Retention, got %s", got)
	}
	if !e.node(e.sec["Ops"]).Retired {
		t.Error("expected Ops retired")
	}

	expectCode(t, e.do(http.MethodPost, "/taxonomy/nodes", bob, url.Values{"parent_id": {"0"}, "level": {"area"}, "name": {"X"}}), http.StatusForbidden)
	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/move", e.sec["Query"]), manager, url.Values{"dir": {"sideways"}}), http.StatusBadRequest)
}

func TestTaxonomySplit(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/split", e.sec["Ingest"]), manager, url.Values{"names": {"Batch ingest\n\n Stream ingest \n"}})
	expectCode(t, rr, http.StatusSeeOther)
	e.expectReclassifyQueued("split")
	if !e.node(e.sec["Ingest"]).Retired {
		t.Error("expected the split Section retired")
	}
	tax, _ := sqlite.LoadTaxonomy(e.db)
	var names []string
	for _, n := range tax.Children(e.sec["Storage"]) {
		names = append(names, n.Name)
	}
	if got := strings.Join(names, ","); got != "Query,Batch ingest,Stream ingest" {
		t.Errorf("expected new sibling Sections, got %s", got)
	}

	// A Category that is itself a Section gains child Sections and stays active.
	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/split", e.sec["Ops"]), manager, url.Values{"names": {"Deploys\nOn-call"}}), http.StatusSeeOther)
	tax, _ = sqlite.LoadTaxonomy(e.db)
	if n, _ := tax.Node(e.sec["Ops"]); n.Retired {
		t.Error("expected the Category to stay active")
	}
	if kids := tax.Children(e.sec["Ops"]); len(kids) != 2 || kids[0].Level != domain.LevelSection {
		t.Errorf("expected two child Sections under Ops, got %+v", kids)
	}
	if _, ok := tax.Section(e.sec["Ops"]); ok {
		t.Error("expected Ops to no longer be a Section")
	}

	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/split", e.sec["Storage"]), manager, url.Values{"names": {"X"}}), http.StatusBadRequest)
	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/split", e.sec["Query"]), manager, url.Values{"names": {"  "}}), http.StatusBadRequest)
}

func TestTaxonomyMerge(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/merge", e.sec["Query"]), manager, url.Values{"into": {fmt.Sprint(e.sec["Ingest"])}})
	expectCode(t, rr, http.StatusSeeOther)
	e.expectReclassifyQueued("merge")
	if e.woke.Load() != 1 {
		t.Errorf("expected wake once, got %d", e.woke.Load())
	}
	if it := e.item("carol"); it.SectionID != e.sec["Ingest"] || it.PlacedBy != domain.PlacedByPerson {
		t.Errorf("expected carol's Work Item merged into Ingest, got %+v", it)
	}
	if !e.node(e.sec["Query"]).Retired {
		t.Error("expected the merged Section retired")
	}
	var reason string
	if err := e.db.QueryRow(`SELECT reason FROM placement_history WHERE work_item_id = ? ORDER BY id DESC LIMIT 1`, e.items["carol"]).Scan(&reason); err != nil || reason != sqlite.ReasonTaxonomyMerge {
		t.Errorf("expected a taxonomy_merge history row, got %q err=%v", reason, err)
	}

	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/merge", e.sec["Ingest"]), manager, url.Values{"into": {fmt.Sprint(e.sec["Ingest"])}}), http.StatusBadRequest)
	expectCode(t, e.do(http.MethodPost, fmt.Sprintf("/taxonomy/nodes/%d/merge", e.sec["Ingest"]), manager, url.Values{"into": {fmt.Sprint(e.sec["Query"])}}), http.StatusBadRequest)
}

func TestLivePage_CutoverBannerWithoutTaxonomy(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.Exec(`DELETE FROM taxonomy_nodes`); err != nil {
		t.Fatal(err)
	}
	rr := e.do(http.MethodGet, "/", alice, nil)
	expectCode(t, rr, http.StatusOK)
	body := rr.Body.String()
	if !strings.Contains(body, "reportbot migrate-v2") || strings.Contains(body, "Fix ingest lag") {
		t.Fatalf("expected only the cut-over banner before the Taxonomy exists:\n%s", body)
	}
}
