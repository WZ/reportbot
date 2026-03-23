package web

import (
	"database/sql"
	"reportbot/internal/config"
	"reportbot/internal/domain"
	llm "reportbot/internal/integrations/llm"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"
	"time"
)

// Type aliases for convenience.
type Config = config.Config
type WorkItem = domain.WorkItem
type ClassificationRecord = domain.ClassificationRecord
type ClassificationCorrection = domain.ClassificationCorrection
type BuildResult = report.BuildResult
type LLMSectionDecision = llm.LLMSectionDecision
type ReportTemplate = report.ReportTemplate

// Database delegates — package-level function vars for testability.
var GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	return sqlite.GetItemsByDateRange(db, from, to)
}

var GetWorkItemByID = func(db *sql.DB, id int64) (WorkItem, error) {
	return sqlite.GetWorkItemByID(db, id)
}

var GetLatestClassification = func(db *sql.DB, workItemID int64) (ClassificationRecord, error) {
	return sqlite.GetLatestClassification(db, workItemID)
}

var GetLatestClassificationsForItems = func(db *sql.DB, itemIDs []int64) (map[int64]ClassificationRecord, error) {
	return sqlite.GetLatestClassificationsForItems(db, itemIDs)
}

var GetRecentCorrections = func(db *sql.DB, since time.Time, limit int) ([]ClassificationCorrection, error) {
	return sqlite.GetRecentCorrections(db, since, limit)
}

var ReclassifyItem = func(db *sql.DB, c ClassificationCorrection, newCategory string) error {
	return sqlite.ReclassifyItem(db, c, newCategory)
}

var UpdateWorkItemTextAndStatus = func(db *sql.DB, id int64, desc, status string) error {
	return sqlite.UpdateWorkItemTextAndStatus(db, id, desc, status)
}

var DeleteWorkItemByID = func(db *sql.DB, id int64) error {
	return sqlite.DeleteWorkItemByID(db, id)
}

// Report delegates.
var BuildReportsFromLast = func(cfg Config, items []WorkItem, reportDate time.Time, corrections []ClassificationCorrection, historicalItems []domain.HistoricalItem) (BuildResult, error) {
	return report.BuildReportsFromLast(cfg, items, reportDate, corrections, historicalItems)
}

var RenderMarkdownByMode = func(t *ReportTemplate, mode string) string {
	return report.RenderMarkdownByMode(t, mode)
}

var WriteReportFile = func(content, outputDir string, friday time.Time, teamName string) (string, error) {
	return report.WriteReportFile(content, outputDir, friday, teamName)
}

// Config delegates.
var IsManagerID = func(cfg Config, userID string) bool {
	return cfg.IsManagerID(userID)
}

// Domain helpers.
var ReportWeekRange = func(cfg Config, now time.Time) (time.Time, time.Time) {
	return domain.ReportWeekRange(cfg, now)
}

// GetClassifiedItemsWithSections for historical example selection.
var GetClassifiedItemsWithSections = func(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
	return sqlite.GetClassifiedItemsWithSections(db, since, limit)
}
