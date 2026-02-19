package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "reportbot-test.db")
	db, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestInitDBAddsAuthorIDColumn(t *testing.T) {
	db := newTestDB(t)

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('work_items') WHERE name = 'author_id'`).Scan(&count); err != nil {
		t.Fatalf("query pragma_table_info failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected author_id column to exist, count=%d", count)
	}
}

func TestWorkItemCRUDAndQueries(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Truncate(time.Second)

	item1 := WorkItem{
		Description: "Implement feature A",
		Author:      "Alice",
		AuthorID:    "U001",
		Source:      "slack",
		Status:      "in progress",
		ReportedAt:  base,
	}
	if err := InsertWorkItem(db, item1); err != nil {
		t.Fatalf("InsertWorkItem failed: %v", err)
	}

	items := []WorkItem{
		{
			Description: "Fix bug B",
			Author:      "Alice",
			AuthorID:    "U001",
			Source:      "slack",
			Status:      "done",
			ReportedAt:  base.Add(10 * time.Minute),
		},
		{
			Description: "MR title",
			Author:      "Bob",
			AuthorID:    "",
			Source:      "gitlab",
			SourceRef:   "https://gitlab.example.com/group/proj/-/merge_requests/1",
			Status:      "done",
			ReportedAt:  base.Add(20 * time.Minute),
		},
	}
	inserted, err := InsertWorkItems(db, items)
	if err != nil {
		t.Fatalf("InsertWorkItems failed: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("expected inserted=2, got %d", inserted)
	}
	if err := InsertWorkItem(db, WorkItem{
		Description: "Legacy Slack entry",
		Author:      "Charlie",
		AuthorID:    "",
		Source:      "slack",
		Status:      "done",
		ReportedAt:  base.Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("InsertWorkItem legacy slack failed: %v", err)
	}

	exists, err := SourceRefExists(db, "https://gitlab.example.com/group/proj/-/merge_requests/1")
	if err != nil {
		t.Fatalf("SourceRefExists failed: %v", err)
	}
	if !exists {
		t.Fatal("expected SourceRefExists to return true")
	}

	from := base.Add(-1 * time.Hour)
	to := base.Add(2 * time.Hour)
	all, err := GetItemsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 items, got %d", len(all))
	}

	idByDesc := make(map[string]int64, len(all))
	for _, it := range all {
		idByDesc[it.Description] = it.ID
	}

	slackItems, err := GetSlackItemsByAuthorAndDateRange(db, "Alice", from, to)
	if err != nil {
		t.Fatalf("GetSlackItemsByAuthorAndDateRange failed: %v", err)
	}
	if len(slackItems) != 2 {
		t.Fatalf("expected 2 Slack items for Alice, got %d", len(slackItems))
	}

	pending, err := GetPendingSlackItemsByAuthorAndDateRange(db, "Alice", from, to)
	if err != nil {
		t.Fatalf("GetPendingSlackItemsByAuthorAndDateRange failed: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending item for Alice, got %d", len(pending))
	}
	if pending[0].Description != "Implement feature A" {
		t.Fatalf("unexpected pending item: %q", pending[0].Description)
	}

	authors, err := GetSlackAuthorsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetSlackAuthorsByDateRange failed: %v", err)
	}
	if !authors["Alice"] {
		t.Fatal("expected Alice in slack authors map")
	}
	if authors["Bob"] {
		t.Fatal("did not expect Bob in slack authors map")
	}

	authorIDs, err := GetSlackAuthorIDsByDateRange(db, from, to)
	if err != nil {
		t.Fatalf("GetSlackAuthorIDsByDateRange failed: %v", err)
	}
	if !authorIDs["U001"] {
		t.Fatal("expected U001 in slack author_id map")
	}
	if len(authorIDs) != 1 {
		t.Fatalf("expected only one non-empty slack author_id, got %d", len(authorIDs))
	}

	updateID := idByDesc["Implement feature A"]
	if err := UpdateWorkItemTextAndStatus(db, updateID, "Implement feature A v2", "in testing"); err != nil {
		t.Fatalf("UpdateWorkItemTextAndStatus failed: %v", err)
	}
	if err := UpdateCategories(db, map[int64]string{updateID: "S0_0"}); err != nil {
		t.Fatalf("UpdateCategories failed: %v", err)
	}
	if err := UpdateTicketIDs(db, map[int64]string{updateID: "123456"}); err != nil {
		t.Fatalf("UpdateTicketIDs failed: %v", err)
	}
	if err := UpdateWorkItemCategory(db, updateID, "S1_0"); err != nil {
		t.Fatalf("UpdateWorkItemCategory failed: %v", err)
	}

	updated, err := GetWorkItemByID(db, updateID)
	if err != nil {
		t.Fatalf("GetWorkItemByID failed: %v", err)
	}
	if updated.Description != "Implement feature A v2" {
		t.Fatalf("unexpected updated description: %q", updated.Description)
	}
	if updated.Status != "in testing" {
		t.Fatalf("unexpected updated status: %q", updated.Status)
	}
	if updated.TicketIDs != "123456" {
		t.Fatalf("unexpected ticket IDs: %q", updated.TicketIDs)
	}
	if updated.Category != "S1_0" {
		t.Fatalf("unexpected category: %q", updated.Category)
	}

	deleteID := idByDesc["Fix bug B"]
	if err := DeleteWorkItemByID(db, deleteID); err != nil {
		t.Fatalf("DeleteWorkItemByID failed: %v", err)
	}
	if _, err := GetWorkItemByID(db, deleteID); err == nil {
		t.Fatal("expected deleted item lookup to fail")
	}
}

func TestClassificationHistoryCorrectionsAndStats(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	workItems := []WorkItem{
		{
			Description: "Improve API throughput",
			Author:      "Alex",
			AuthorID:    "U100",
			Source:      "slack",
			Status:      "done",
			ReportedAt:  now,
		},
		{
			Description: "Refactor dashboard cache",
			Author:      "Casey",
			AuthorID:    "U200",
			Source:      "slack",
			Status:      "in progress",
			ReportedAt:  now.Add(1 * time.Minute),
		},
	}
	if _, err := InsertWorkItems(db, workItems); err != nil {
		t.Fatalf("InsertWorkItems failed: %v", err)
	}

	all, err := GetItemsByDateRange(db, now.Add(-1*time.Hour), now.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("GetItemsByDateRange failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 items, got %d", len(all))
	}

	id1 := all[0].ID
	id2 := all[1].ID

	history := []ClassificationRecord{
		{
			WorkItemID:       id1,
			SectionID:        "S0_0",
			SectionLabel:     "Backend",
			Confidence:       0.93,
			NormalizedStatus: "done",
			TicketIDs:        "111",
			DuplicateOf:      "",
			LLMProvider:      "openai",
			LLMModel:         "gpt-test",
		},
		{
			WorkItemID:       id2,
			SectionID:        "S1_0",
			SectionLabel:     "Infrastructure",
			Confidence:       0.64,
			NormalizedStatus: "in progress",
			TicketIDs:        "",
			DuplicateOf:      "",
			LLMProvider:      "openai",
			LLMModel:         "gpt-test",
		},
	}
	if err := InsertClassificationHistory(db, history); err != nil {
		t.Fatalf("InsertClassificationHistory failed: %v", err)
	}

	latest, err := GetLatestClassification(db, id1)
	if err != nil {
		t.Fatalf("GetLatestClassification failed: %v", err)
	}
	if latest.SectionID != "S0_0" {
		t.Fatalf("unexpected latest section: %q", latest.SectionID)
	}

	if err := InsertClassificationCorrection(db, ClassificationCorrection{
		WorkItemID:         id1,
		OriginalSectionID:  "S1_0",
		OriginalLabel:      "Infrastructure",
		CorrectedSectionID: "S0_0",
		CorrectedLabel:     "Backend",
		Description:        "Improve API throughput",
		CorrectedBy:        "U999",
	}); err != nil {
		t.Fatalf("InsertClassificationCorrection #1 failed: %v", err)
	}
	if err := InsertClassificationCorrection(db, ClassificationCorrection{
		WorkItemID:         id2,
		OriginalSectionID:  "S1_0",
		OriginalLabel:      "Infrastructure",
		CorrectedSectionID: "S2_0",
		CorrectedLabel:     "Data",
		Description:        "Refactor dashboard cache",
		CorrectedBy:        "U999",
	}); err != nil {
		t.Fatalf("InsertClassificationCorrection #2 failed: %v", err)
	}

	since := now.Add(-24 * time.Hour)

	corrections, err := GetRecentCorrections(db, since, 10)
	if err != nil {
		t.Fatalf("GetRecentCorrections failed: %v", err)
	}
	if len(corrections) != 2 {
		t.Fatalf("expected 2 corrections, got %d", len(corrections))
	}

	count, err := CountCorrectionsByPhrase(db, "Improve API throughput", "S0_0")
	if err != nil {
		t.Fatalf("CountCorrectionsByPhrase failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected count=1, got %d", count)
	}

	historical, err := GetClassifiedItemsWithSections(db, since, 10)
	if err != nil {
		t.Fatalf("GetClassifiedItemsWithSections failed: %v", err)
	}
	if len(historical) == 0 {
		t.Fatal("expected at least one historical item")
	}

	stats, err := GetClassificationStats(db, since)
	if err != nil {
		t.Fatalf("GetClassificationStats failed: %v", err)
	}
	if stats.TotalClassifications != 2 {
		t.Fatalf("expected total classifications=2, got %d", stats.TotalClassifications)
	}
	if stats.TotalCorrections != 2 {
		t.Fatalf("expected total corrections=2, got %d", stats.TotalCorrections)
	}

	bySection, err := GetCorrectionsBySection(db, since)
	if err != nil {
		t.Fatalf("GetCorrectionsBySection failed: %v", err)
	}
	if len(bySection) == 0 {
		t.Fatal("expected corrections by section to be non-empty")
	}

	trend, err := GetWeeklyClassificationTrend(db, since)
	if err != nil {
		t.Fatalf("GetWeeklyClassificationTrend failed: %v", err)
	}
	if len(trend) == 0 {
		t.Fatal("expected weekly trend to be non-empty")
	}
}
