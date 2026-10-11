package slackbot

import (
	"database/sql"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/fetch"
	"reportbot/internal/nudge"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

type Config = config.Config
type WorkItem = domain.WorkItem
type RenderedNudge = nudge.RenderedNudge

const (
	actionNudgeDone     = nudge.ActionDone
	actionNudgeMore     = nudge.ActionMore
	actionNudgePagePrev = nudge.ActionPagePrev
	actionNudgePageNext = nudge.ActionPageNext
)

func ReportWeekRange(cfg Config, now time.Time) (time.Time, time.Time) {
	return domain.ReportWeekRange(cfg, now)
}

func FridayOfWeek(monday time.Time) time.Time {
	return domain.FridayOfWeek(monday)
}

func FetchAndImportMRs(cfg Config, db *sql.DB, hooks fetch.Hooks) (fetch.FetchResult, error) {
	return fetch.FetchAndImportMRs(cfg, db, hooks)
}

func FormatFetchSummary(result fetch.FetchResult) string {
	return fetch.FormatFetchSummary(result)
}

func synthesizeName(name string) string {
	return report.SynthesizeName(name)
}

func GetItemsByDateRange(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	return sqlite.GetItemsByDateRange(db, from, to)
}

func GetWeekItems(db *sql.DB, from, to time.Time) ([]WorkItem, error) {
	return sqlite.GetWeekItems(db, from, to)
}

func GetOpenItems(db *sql.DB) ([]WorkItem, error) {
	return sqlite.GetOpenItems(db)
}

func GetSlackAuthorIDsByDateRange(db *sql.DB, from, to time.Time) (map[string]bool, error) {
	return sqlite.GetSlackAuthorIDsByDateRange(db, from, to)
}

func GetWorkItemByID(db *sql.DB, id int64) (WorkItem, error) {
	return sqlite.GetWorkItemByID(db, id)
}

func SetWorkItemStatus(db *sql.DB, id int64, status string) error {
	return sqlite.SetWorkItemStatus(db, id, status)
}

func DeleteWorkItemByID(db *sql.DB, id int64) error {
	return sqlite.DeleteWorkItemByID(db, id)
}

func LoadTaxonomy(db *sql.DB) (domain.Taxonomy, error) {
	return sqlite.LoadTaxonomy(db)
}

func sendNudges(api *slack.Client, db *sql.DB, cfg Config, memberIDs []string, reportChannelID string) {
	nudge.SendNudges(api, db, cfg, memberIDs, reportChannelID)
}

func RenderNudgeForUser(api *slack.Client, db *sql.DB, cfg Config, userID, reportChannelID string, now time.Time, page int, updated bool) (RenderedNudge, error) {
	return nudge.RenderNudgeForUser(api, db, cfg, userID, reportChannelID, now, page, updated)
}

type Placement = sqlite.Placement
type WeekStats = sqlite.WeekStats

const (
	reasonButton = sqlite.ReasonButton
	reasonEdit   = sqlite.ReasonEdit
	reasonUndo   = sqlite.ReasonUndo
)

// Storage seams, swappable in tests.
var (
	insertWorkItemsForFiling = sqlite.InsertWorkItemsForFiling
	updateWorkItemFields     = sqlite.UpdateWorkItemFields
	setFilingPending         = sqlite.SetFilingPending
	setPlacement             = sqlite.SetPlacement
	applyUpdate              = sqlite.ApplyUpdate
	getPlacementEvent        = sqlite.GetPlacementEvent
	getCodeChange            = sqlite.GetCodeChange
	getCodeChangesForItem    = sqlite.GetCodeChangesForItem
	confirmCodeChangeLink    = sqlite.ConfirmCodeChangeLink
	rejectCodeChangeLink     = sqlite.RejectCodeChangeLink
	getRecentCorrections     = sqlite.GetRecentCorrections
	getWeekStats             = sqlite.GetWeekStats
	setSectionDescription    = sqlite.SetSectionDescription
	enqueueReclassify        = sqlite.EnqueueReclassify
)
