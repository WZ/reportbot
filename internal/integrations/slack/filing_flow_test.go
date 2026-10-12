package slackbot

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
	sqlitedb "reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

// slackRecorder fakes the Slack Web API and response URLs, recording what was sent.
type slackRecorder struct {
	mu        sync.Mutex
	calls     []string     // API method names
	forms     []url.Values // their form bodies
	responses []map[string]any
	srv       *httptest.Server
}

func newSlackRecorder(t *testing.T) (*slack.Client, *slackRecorder) {
	t.Helper()
	resetUserCacheForTest(t)
	rec := &slackRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if r.URL.Path == "/respond" {
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &m)
			rec.responses = append(rec.responses, m)
			w.Write([]byte("ok"))
			return
		}
		_ = r.ParseForm()
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		rec.calls = append(rec.calls, method)
		rec.forms = append(rec.forms, r.Form)
		switch method {
		case "users.info":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": r.Form.Get("user"), "name": "dev",
				"real_name": "Dev A", "profile": map[string]any{"display_name": "Dev A"}}})
		case "conversations.open":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D1"}})
		case "chat.postMessage":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "D1", "ts": "1.2"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(rec.srv.Close)
	return slack.New("xoxb-test", slack.OptionAPIURL(rec.srv.URL+"/api/")), rec
}

func (r *slackRecorder) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (r *slackRecorder) lastText(method string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i] == method {
			return r.forms[i].Get("text") + r.forms[i].Get("blocks")
		}
	}
	return ""
}

func seedSections(t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()
	ids, err := sqlitedb.SeedTaxonomy(db, []sqlitedb.SeedArea{{Name: "Product A", Categories: []sqlitedb.SeedCategory{
		{Name: "Engineering", Sections: []string{"Cluster Manager"}}, {Name: "Infrastructure"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return ids[sqlitedb.SeedKey("Product A", "Engineering", "Cluster Manager")], ids[sqlitedb.SeedKey("Product A", "Infrastructure", "")]
}

func TestHandleReportSavesThenQueuesFiling(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	woke := 0
	wakeFiling = func() { woke++ }
	t.Cleanup(func() { wakeFiling = func() {} })

	handleReport(api, db, Config{Location: time.UTC}, slack.SlashCommand{Command: "/report", UserID: "U1", ChannelID: "C1",
		Text: "[1234567] Fix shard timeout (root cause analysis in progress)"})

	items, _ := sqlitedb.GetOpenItems(db)
	if len(items) != 1 {
		t.Fatalf("expected the item saved before filing, got %d", len(items))
	}
	it := items[0]
	if it.FilingState != domain.FilingPending || it.Status != "" || it.StatusNote != "root cause analysis in progress" || it.TicketIDs != "1234567" || it.AuthorID != "U1" {
		t.Fatalf("unexpected saved item: %+v", it)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE work_item_id = ? AND state = 'queued'`, it.ID).Scan(&n)
	if n != 1 || woke != 1 {
		t.Fatalf("expected one queued filing job and a wake, got jobs=%d wakes=%d", n, woke)
	}
	if !strings.Contains(rec.lastText("chat.postEphemeral"), "Saved — filing it") {
		t.Fatalf("expected the immediate reply, got %q", rec.lastText("chat.postEphemeral"))
	}
}

func TestNotifierRepliesThroughResponseURL(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	cm, _ := seedSections(t, db)
	tax, _ := sqlitedb.LoadTaxonomy(db)
	sec, _ := tax.Section(cm)
	item := domain.WorkItem{ID: 9, Author: "dev a", Description: "fix shard timeout", Status: domain.StatusInProgress, TicketIDs: "1234567", SectionID: cm}

	n := NewNotifier(api, db, Config{WebEnabled: true, WebBaseURL: "https://report.example"})
	n.Filed(filing.Result{Item: item, Section: &sec, Payload: filing.Payload{Kind: "arrival", ResponseURL: rec.srv.URL + "/respond", Replace: true}})

	if len(rec.responses) != 1 {
		t.Fatalf("expected one response_url reply, got %d", len(rec.responses))
	}
	got := rec.responses[0]
	b, _ := json.Marshal(got["blocks"])
	if got["replace_original"] != true || !strings.Contains(got["text"].(string), "Filed under *Cluster Manager*: [1234567] Fix shard timeout") {
		t.Fatalf("unexpected reply: %v", got)
	}
	for _, want := range []string{"Filed under *Cluster Manager*", "\\u003e [1234567] Fix shard timeout", "\\u003e _in progress_", "Product A › Engineering"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
	if !strings.Contains(string(b), "https://report.example/") {
		t.Fatalf("expected the live report link: %s", b)
	}
}

func TestFiledBlocksAskAndUpdate(t *testing.T) {
	item := domain.WorkItem{ID: 9, Author: "Dev A", Description: "x", Status: domain.StatusInProgress}
	choices := []domain.Section{{ID: 3, Area: "A", Category: "C", Name: "S1"}, {ID: 5, Area: "A", Category: "Infra"}}
	upd := filing.Candidate{Item: domain.WorkItem{ID: 4, Author: "Dev A", Description: "older", Status: domain.StatusInProgress}}
	text, blocks := FiledBlocks(Config{}, filing.Result{Item: item, Choices: choices, Update: &upd})
	b, _ := json.Marshal(blocks)
	for _, blk := range blocks {
		ab, ok := blk.(*slack.ActionBlock)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, el := range ab.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok {
				if seen[btn.ActionID] {
					t.Fatalf("Slack rejects duplicate action_id %q in block %q", btn.ActionID, ab.BlockID)
				}
				seen[btn.ActionID] = true
			}
		}
	}
	for _, want := range []string{`"action_id":"place_pick:0","value":"9:3"`, `"action_id":"place_pick:1","value":"9:5"`,
		`"action_id":"place_other","value":"9"`, `"action_id":"update_confirm","value":"4:9"`, `"action_id":"update_reject","value":"4:9"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if !strings.Contains(text, "Where does this go?") || !strings.Contains(text, "Is this an update to an earlier Work Item?") {
		t.Fatalf("unexpected text: %s", text)
	}
	for _, want := range []string{"Yes, update it", "No, keep both", "\"style\":\"primary\"", "Updating replaces the earlier Work Item"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
	kept := domain.Section{ID: 5, Area: "A", Category: "Infra"}
	_, keptBlocks := FiledBlocks(Config{}, filing.Result{Item: item, Choices: choices, Kept: &kept})
	if kb, _ := json.Marshal(keptBlocks); !strings.Contains(string(kb), "It stays in A › Infra until someone picks.") {
		t.Fatalf("an unsure edit keeps its Section and says so: %s", kb)
	}
	failed, _ := FiledBlocks(Config{}, filing.Result{Item: item, Failed: true})
	if !strings.Contains(failed, "model unreachable") {
		t.Fatalf("unexpected failure text: %s", failed)
	}
}

func interaction(user, actionID, value, responseURL string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions, User: slack.User{ID: user}, ResponseURL: responseURL,
		Channel:        slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{ActionID: actionID, Value: value}}},
	}
}

func TestPlacePickUpdateUndoAndCheckDone(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	cm, infra := seedSections(t, db)
	cfg := Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}}
	insert := func(desc string, section int64) int64 {
		id, err := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Source: "slack",
			Status: domain.StatusInProgress, ReportedAt: time.Now(), SectionID: section, FilingState: domain.FilingFiled, PlacedBy: domain.PlacedByModel})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// "Where does this go?": only the owner (or a manager) may pick.
	asked := insert("rack power", 0)
	handleBlockActions(api, db, cfg, interaction("U2", actionPlacePick+":0", itoa(asked)+":"+itoa(infra), rec.srv.URL+"/respond"))
	if it, _ := sqlitedb.GetWorkItemByID(db, asked); it.SectionID != 0 {
		t.Fatal("another member must not file someone else's item")
	}
	handleBlockActions(api, db, cfg, interaction("U1", actionPlacePick+":0", itoa(asked)+":"+itoa(infra), rec.srv.URL+"/respond"))
	if it, _ := sqlitedb.GetWorkItemByID(db, asked); it.SectionID != infra || it.PlacedBy != domain.PlacedByPerson {
		t.Fatalf("expected a person Placement: %+v", it)
	}

	// [Update it] merges the new report into the existing item.
	existing := insert("shard timeout", cm)
	newer, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "shard timeout fixed", Author: "Dev A", AuthorID: "U1", Source: "slack",
		Status: domain.StatusInTesting, ReportedAt: time.Now()})
	handleBlockActions(api, db, cfg, interaction("U1", actionUpdateConfirm, itoa(existing)+":"+itoa(newer), rec.srv.URL+"/respond"))
	if it, _ := sqlitedb.GetWorkItemByID(db, existing); it.Description != "shard timeout fixed" || it.Status != domain.StatusInTesting {
		t.Fatalf("expected the update applied: %+v", it)
	}
	if _, err := sqlitedb.GetWorkItemByID(db, newer); err != sql.ErrNoRows {
		t.Fatal("the new report must not stay a separate item")
	}

	// Undo restores the Placement a re-classification replaced.
	if err := sqlitedb.SetPlacement(db, asked, sqlitedb.Placement{SectionID: cm, PlacedBy: domain.PlacedByModel, HistoryNote: sqlitedb.ReasonReclassify}); err != nil {
		t.Fatal(err)
	}
	ev, _ := sqlitedb.LatestPlacementEventID(db, asked)
	handleBlockActions(api, db, cfg, interaction("U1", actionPlacementUndo, itoa(ev), rec.srv.URL+"/respond"))
	if it, _ := sqlitedb.GetWorkItemByID(db, asked); it.SectionID != infra || it.PlacedBy != domain.PlacedByPerson {
		t.Fatalf("expected undo back to the person's Section: %+v", it)
	}

	// /check's Done button is manager-only.
	handleBlockActions(api, db, cfg, interaction("U1", actionCheckDone, itoa(existing), ""))
	if it, _ := sqlitedb.GetWorkItemByID(db, existing); it.Status == domain.StatusDone {
		t.Fatal("members cannot use the /check Done button")
	}
	handleBlockActions(api, db, cfg, interaction("UMGR", actionCheckDone, itoa(existing), ""))
	if it, _ := sqlitedb.GetWorkItemByID(db, existing); it.Status != domain.StatusDone || it.DoneAt.IsZero() {
		t.Fatalf("expected done with done_at: %+v", it)
	}
	if len(rec.responses) < 3 {
		t.Fatalf("expected button results to replace their messages, got %d replies", len(rec.responses))
	}
}

func TestMergedInTestingBlocksListsCodeChanges(t *testing.T) {
	db := newTestDB(t)
	id, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "exporter", Author: "Dev B", Source: "gitlab",
		Status: domain.StatusInTesting, ReportedAt: time.Now(), FilingState: domain.FilingFiled})
	sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "slack item", Author: "Dev B", Source: "slack",
		Status: domain.StatusInTesting, ReportedAt: time.Now(), FilingState: domain.FilingFiled})
	b, _ := json.Marshal(mergedInTestingBlocks(db))
	if !strings.Contains(string(b), `"value":"`+itoa(id)+`"`) || strings.Contains(string(b), "slack item") || !strings.Contains(string(b), "waiting in testing (1)") {
		t.Fatalf("unexpected /check blocks: %s", b)
	}
}

func TestEditSubmitRefilesOnTextChangeAndPlacesOnSectionChoice(t *testing.T) {
	db := newTestDB(t)
	api, _ := newSlackRecorder(t)
	cm, infra := seedSections(t, db)
	woke := 0
	wakeFiling = func() { woke++ }
	t.Cleanup(func() { wakeFiling = func() {} })
	id, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "old text", Author: "Dev A", AuthorID: "U1", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: time.Now(), SectionID: cm, FilingState: domain.FilingFiled, PlacedBy: domain.PlacedByModel})

	submit := func(desc, status, note, section string) {
		cb := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission, User: slack.User{ID: "U1"}, View: slack.View{
			CallbackID: modalEditCallbackID, PrivateMetadata: modalMetaPrefix + "mine:" + itoa(id) + "|C1",
			State: &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
				editBlockDescription: {editActionDescription: {Value: desc}},
				editBlockStatus:      {editActionStatus: {SelectedOption: slack.OptionBlockObject{Value: status}}},
				editBlockStatusNote:  {editActionStatusNote: {Value: note}},
				editBlockSection:     {editActionSection: {SelectedOption: slack.OptionBlockObject{Value: section}}},
			}}}}
		handleViewSubmission(api, db, Config{Location: time.UTC}, cb)
	}

	submit("new text", domain.StatusBlocked, "waiting on vendor", itoa(cm))
	it, _ := sqlitedb.GetWorkItemByID(db, id)
	if it.Description != "new text" || it.Status != domain.StatusBlocked || it.StatusNote != "waiting on vendor" || it.FilingState != domain.FilingPending || woke != 1 {
		t.Fatalf("a text change re-files the item: %+v wakes=%d", it, woke)
	}

	submit("new text", domain.StatusBlocked, "waiting on vendor", itoa(infra))
	it, _ = sqlitedb.GetWorkItemByID(db, id)
	if it.SectionID != infra || it.PlacedBy != domain.PlacedByPerson || woke != 1 {
		t.Fatalf("a chosen Section is a person Placement without re-filing: %+v wakes=%d", it, woke)
	}
}

func TestPublishWarnsAboutUndetermined(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	seedSections(t, db)
	cfg := Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}, MondayCutoffTime: "12:00", ReportOutputDir: t.TempDir(), TeamName: "T"}
	sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "unsure", Author: "Dev A", Source: "slack",
		Status: domain.StatusInProgress, ReportedAt: inReportWeek(cfg), FilingState: domain.FilingAsking})

	handleGenerateReport(api, db, cfg, slack.SlashCommand{Command: "/generate-report", UserID: "UMGR", ChannelID: "C1"})
	if got := rec.lastText("chat.postEphemeral"); !strings.Contains(got, "still in Undetermined") || !strings.Contains(got, actionPublishAnyway) {
		t.Fatalf("expected a warning with Publish anyway, got %q", got)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM published_reports`).Scan(&n)
	if n != 0 {
		t.Fatal("nothing is published before the manager confirms")
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// The fake users.info returns display name "Dev A" for everyone, so a name match is always available.
func TestItemsWithoutAuthorIDAreManagerOnly(t *testing.T) {
	db := newTestDB(t)
	api, _ := newSlackRecorder(t)
	cm, infra := seedSections(t, db)
	cfg := Config{Location: time.UTC, ManagerSlackIDs: []string{"UMGR"}}
	id, _ := sqlitedb.InsertWorkItem(db, domain.WorkItem{Description: "imported line", Author: "Dev A", Source: "import",
		Status: domain.StatusInProgress, ReportedAt: time.Now(), SectionID: cm, FilingState: domain.FilingFiled, PlacedBy: domain.PlacedByPerson})

	handleBlockActions(api, db, cfg, interaction("U1", actionPlacePick+":1", itoa(id)+":"+itoa(infra), ""))
	if err := sqlitedb.SetPlacement(db, id, sqlitedb.Placement{SectionID: infra, PlacedBy: domain.PlacedByModel, HistoryNote: sqlitedb.ReasonReclassify}); err != nil {
		t.Fatal(err)
	}
	ev, _ := sqlitedb.LatestPlacementEventID(db, id)
	handleBlockActions(api, db, cfg, interaction("U1", actionPlacementUndo, itoa(ev), ""))
	handleBlockActions(api, db, cfg, interaction("U1", actionNudgeDone, "done|U1|"+itoa(id)+"|0", ""))
	deleteItemAction(api, db, cfg, "C1", "U1", id, listScopeMine)
	it, err := sqlitedb.GetWorkItemByID(db, id)
	if err != nil || it.SectionID != infra || it.Status != domain.StatusInProgress {
		t.Fatalf("a display-name match must not change the item: %+v err=%v", it, err)
	}

	handleBlockActions(api, db, cfg, interaction("UMGR", actionPlacementUndo, itoa(ev), ""))
	if it, _ := sqlitedb.GetWorkItemByID(db, id); it.SectionID != cm {
		t.Fatalf("a manager may act on items without an author ID: %+v", it)
	}
	if canManageItem(it, false, "U1") || !canManageItem(domain.WorkItem{AuthorID: "U1"}, false, "U1") {
		t.Fatal("ownership is by Slack user ID only")
	}
}

func TestReportSaveFailureSaysSoAndNothingElse(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	orig := insertWorkItemsForFiling
	insertWorkItemsForFiling = func(*sql.DB, []WorkItem, string) ([]int64, error) { return nil, fmt.Errorf("disk full") }
	t.Cleanup(func() { insertWorkItemsForFiling = orig })
	handleReport(api, db, Config{Location: time.UTC}, slack.SlashCommand{Command: "/report", UserID: "U1", ChannelID: "C1", Text: "x (done)"})
	if got := rec.lastText("chat.postEphemeral"); strings.Contains(got, "Saved") || !strings.Contains(got, "disk full") {
		t.Fatalf("expected only the error reply, got %q", got)
	}
}

func TestLinkSeparateShowsSectionButtonsWhenUnsure(t *testing.T) {
	db := newTestDB(t)
	api, rec := newSlackRecorder(t)
	cm, infra := seedSections(t, db)
	ccID, itemID, err := sqlitedb.InsertCodeChangeWithItem(db, domain.CodeChange{Source: "gitlab", SourceRef: "https://git.example/mr/5", Title: "fix",
		Author: "Dev A", AuthorID: "U1", State: "open"}, domain.WorkItem{Description: "fix", Author: "Dev A", AuthorID: "U1", Source: "gitlab",
		Status: domain.StatusInProgress, ReportedAt: time.Now()}, "")
	if err != nil {
		t.Fatal(err)
	}
	handleBlockActions(api, db, Config{}, interaction("U1", actionLinkReject, itoa(ccID)+"|"+itoa(cm)+","+itoa(infra), rec.srv.URL+"/respond"))
	if len(rec.responses) != 1 {
		t.Fatalf("expected one reply, got %d", len(rec.responses))
	}
	b, _ := json.Marshal(rec.responses[0])
	for _, want := range []string{`"value":"` + itoa(itemID) + ":" + itoa(cm), `"value":"` + itoa(itemID) + ":" + itoa(infra), "Where does this go?", "own Work Item"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
}

// inReportWeek is a time inside the week /generate-report publishes now, whatever the clock says.
func inReportWeek(cfg Config) time.Time {
	monday, _ := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	return monday.Add(time.Hour)
}
