package sqlite

import (
	"encoding/json"
	"testing"
	"time"

	"reportbot/internal/domain"
)

func TestEditsAreRecordedWithBeforeAfterWhoAndWhen(t *testing.T) {
	db := newTestDB(t)
	add := func(desc, tickets string) int64 {
		id, err := InsertWorkItem(db, WorkItem{Description: desc, Author: "Dev A", AuthorID: "U1", Source: "slack",
			Status: domain.StatusInProgress, TicketIDs: tickets, ReportedAt: time.Now(), FilingState: domain.FilingFiled})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := add("fix shard", "1111111")
	web := Actor{ID: "U1", Via: "web"}

	if err := UpdateWorkItemFields(db, a, "fix shard timeout", domain.StatusBlocked, "vendor", "1111111", web); err != nil {
		t.Fatal(err)
	}
	if err := UpdateWorkItemFields(db, a, "fix shard timeout", domain.StatusBlocked, "vendor", "1111111", web); err != nil {
		t.Fatal(err)
	}
	if err := SetWorkItemStatus(db, a, domain.StatusDone, Actor{ID: "U2", Via: "slack"}); err != nil {
		t.Fatal(err)
	}
	edits, err := GetWorkItemEdits(db, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 {
		t.Fatalf("a no-op save is not an edit; want 2 edits, got %+v", edits)
	}
	e := edits[0]
	if e.Kind != EditFields || e.ActorID != "U1" || e.Via != "web" || e.CreatedAt.IsZero() ||
		e.Before == nil || e.Before.Description != "fix shard" || e.After == nil || e.After.Description != "fix shard timeout" ||
		e.After.Status != domain.StatusBlocked || e.After.StatusNote != "vendor" {
		t.Fatalf("text edit: %+v before=%+v after=%+v", e, e.Before, e.After)
	}
	if s := edits[1]; s.Kind != EditStatus || s.Before.Status != domain.StatusBlocked || s.After.Status != domain.StatusDone || s.ActorID != "U2" {
		t.Fatalf("status edit: %+v", s)
	}

	// An Update merge records the existing item before and after, and the report it absorbed.
	b := add("old text", "2222222")
	report := add("new text", "3333333")
	if _, err := ApplyUpdate(db, b, report, Actor{ID: "U1", Via: "slack"}); err != nil {
		t.Fatal(err)
	}
	edits, _ = GetWorkItemEdits(db, b)
	if len(edits) != 1 || edits[0].Kind != EditUpdateMerge || edits[0].RelatedItemID != report ||
		edits[0].Before.Description != "old text" || edits[0].After.Description != "new text" || edits[0].After.TicketIDs != "2222222,3333333" {
		t.Fatalf("update merge: %+v", edits)
	}

	// A delete records what was deleted; the history outlives the Work Item.
	c := add("to delete", "")
	if err := DeleteWorkItemByID(db, c, Actor{ID: "UMGR", Via: "web"}); err != nil {
		t.Fatal(err)
	}
	edits, _ = GetWorkItemEdits(db, c)
	if len(edits) != 1 || edits[0].Kind != EditDelete || edits[0].Before.Description != "to delete" || edits[0].After != nil {
		t.Fatalf("delete: %+v", edits)
	}

	var raw string
	db.QueryRow(`SELECT before_json FROM work_item_edits WHERE kind = 'delete'`).Scan(&raw)
	var snap ItemSnapshot
	if json.Unmarshal([]byte(raw), &snap) != nil || snap.Description != "to delete" {
		t.Fatalf("snapshots are stored as JSON: %q", raw)
	}
}

func TestReportDraftReplacesPerWeek(t *testing.T) {
	db := newTestDB(t)
	if err := SaveReportDraft(db, ReportDraft{WeekStart: "2026-10-05", TeamMarkdown: "t1", BossMarkdown: "b1", ItemCount: 3}); err != nil {
		t.Fatal(err)
	}
	if err := SaveReportDraft(db, ReportDraft{WeekStart: "2026-10-05", TeamMarkdown: "t2", BossMarkdown: "b2", ItemCount: 4, UndeterminedCount: 1}); err != nil {
		t.Fatal(err)
	}
	d, ok, err := GetReportDraft(db, "2026-10-05")
	if err != nil || !ok || d.TeamMarkdown != "t2" || d.ItemCount != 4 || d.UndeterminedCount != 1 || d.CreatedAt.IsZero() {
		t.Fatalf("draft: %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, _ := GetPublishedReport(db, "2026-10-05"); ok {
		t.Fatal("a draft is not a Published Report")
	}
}
