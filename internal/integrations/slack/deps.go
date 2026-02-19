package slackbot

import (
	"database/sql"
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/fetch"
	llm "reportbot/internal/integrations/llm"
	"reportbot/internal/nudge"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"
	"time"

	"github.com/slack-go/slack"
)

type Config = config.Config
type WorkItem = domain.WorkItem
type GitLabMR = domain.GitLabMR
type GitHubPR = domain.GitHubPR
type ClassificationRecord = domain.ClassificationRecord
type ClassificationCorrection = domain.ClassificationCorrection
type ClassificationStats = domain.ClassificationStats
type sectionOption = llm.SectionOption
type BuildResult = report.BuildResult
type LLMSectionDecision = llm.LLMSectionDecision

type loadStatus int

const (
	templateFromFile loadStatus = iota
	templateFirstEver
)

func ReportWeekRange(cfg Config, now time.Time) (time.Time, time.Time) {
	return domain.ReportWeekRange(cfg, now)
}

func FridayOfWeek(monday time.Time) time.Time {
	return domain.FridayOfWeek(monday)
}

func FetchAndImportMRs(cfg Config, db *sql.DB) (fetch.FetchResult, error) {
	return fetch.FetchAndImportMRs(cfg, db)
}

func FormatFetchSummary(result fetch.FetchResult) string {
	return fetch.FormatFetchSummary(result)
}

func BuildReportsFromLast(cfg Config, items []WorkItem, reportDate time.Time, corrections []ClassificationCorrection, historicalItems []domain.HistoricalItem) (BuildResult, error) {
	return report.BuildReportsFromLast(cfg, items, reportDate, corrections, historicalItems)
}

func WriteEmailDraftFile(body, outputDir string, reportDate time.Time, subjectPrefix string) (string, error) {
	return report.WriteEmailDraftFile(body, outputDir, reportDate, subjectPrefix)
}

func WriteReportFile(content, outputDir string, reportDate time.Time, teamName string) (string, error) {
	return report.WriteReportFile(content, outputDir, reportDate, teamName)
}

func renderTeamMarkdown(t *report.ReportTemplate) string {
	return report.RenderTeamMarkdown(t)
}

func renderBossMarkdown(t *report.ReportTemplate) string {
	return report.RenderBossMarkdown(t)
}

func loadTemplateForGeneration(outputDir, teamName string, reportDate time.Time) (*report.ReportTemplate, loadStatus, error) {
	t, err := report.LoadTemplateForGeneration(outputDir, teamName, reportDate)
	if err != nil {
		return nil, templateFromFile, err
	}
	return t, templateFromFile, nil
}

func templateOptions(t *report.ReportTemplate) []sectionOption {
	return report.TemplateOptions(t)
}

func parseTemplate(content string) *report.ReportTemplate {
	return report.ParseTemplate(content)
}

func stripCurrentTeamTitleFromPrefix(t *report.ReportTemplate, teamName string) {
	report.StripCurrentTeamTitleFromPrefix(t, teamName)
}

func normalizeStatus(status string) string {
	return report.NormalizeStatus(status)
}

func synthesizeName(name string) string {
	return report.SynthesizeName(name)
}

func InsertWorkItem(db *sql.DB, item WorkItem) error {
	return sqlite.InsertWorkItem(db, item)
}

func InsertWorkItems(db *sql.DB, items []WorkItem) (int, error) {
	return sqlite.InsertWorkItems(db, items)
}

func GetSlackItemsByAuthorAndDateRange(db *sql.DB, author string, from, to time.Time) ([]WorkItem, error) {
	return sqlite.GetSlackItemsByAuthorAndDateRange(db, author, from, to)
}

func GetItemsByDateRange(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	return sqlite.GetItemsByDateRange(db, from, to)
}

func GetRecentCorrections(db *sql.DB, since time.Time, limit int) ([]ClassificationCorrection, error) {
	return sqlite.GetRecentCorrections(db, since, limit)
}

func GetClassifiedItemsWithSections(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
	return sqlite.GetClassifiedItemsWithSections(db, since, limit)
}

func InsertClassificationHistory(db *sql.DB, records []ClassificationRecord) error {
	return sqlite.InsertClassificationHistory(db, records)
}

func GetSlackAuthorIDsByDateRange(db *sql.DB, from, to time.Time) (map[string]bool, error) {
	return sqlite.GetSlackAuthorIDsByDateRange(db, from, to)
}

func GetWorkItemByID(db *sql.DB, id int64) (WorkItem, error) {
	return sqlite.GetWorkItemByID(db, id)
}

func UpdateWorkItemTextAndStatus(db *sql.DB, id int64, description, status string) error {
	return sqlite.UpdateWorkItemTextAndStatus(db, id, description, status)
}

func UpdateWorkItemCategory(db *sql.DB, id int64, category string) error {
	return sqlite.UpdateWorkItemCategory(db, id, category)
}

func DeleteWorkItemByID(db *sql.DB, id int64) error {
	return sqlite.DeleteWorkItemByID(db, id)
}

func GetClassificationStats(db *sql.DB, since time.Time) (ClassificationStats, error) {
	return sqlite.GetClassificationStats(db, since)
}

func GetCorrectionsBySection(db *sql.DB, since time.Time) ([]sqlite.SectionCorrectionStat, error) {
	return sqlite.GetCorrectionsBySection(db, since)
}

func GetWeeklyClassificationTrend(db *sql.DB, since time.Time) ([]sqlite.WeeklyTrend, error) {
	return sqlite.GetWeeklyClassificationTrend(db, since)
}

func GetLatestClassification(db *sql.DB, workItemID int64) (ClassificationRecord, error) {
	return sqlite.GetLatestClassification(db, workItemID)
}

func InsertClassificationCorrection(db *sql.DB, c ClassificationCorrection) error {
	return sqlite.InsertClassificationCorrection(db, c)
}

func CountCorrectionsByPhrase(db *sql.DB, description, correctedSectionID string) (int, error) {
	return sqlite.CountCorrectionsByPhrase(db, description, correctedSectionID)
}

func analyzeCorrections(cfg Config, corrections []ClassificationCorrection, options []sectionOption) ([]llm.RetroSuggestion, llm.LLMUsage, error) {
	return llm.AnalyzeCorrections(cfg, corrections, options)
}

func AppendGlossaryTerm(path, phrase, section string) error {
	return llm.AppendGlossaryTerm(path, phrase, section)
}

func extractGlossaryPhrase(description string) string {
	return llm.ExtractGlossaryPhrase(description)
}

func sendNudges(api *slack.Client, cfg Config, memberIDs []string, reportChannelID string) {
	nudge.SendNudges(api, cfg, memberIDs, reportChannelID)
}

func normalizeTextToken(s string) string {
	return llm.NormalizeTextToken(s)
}
