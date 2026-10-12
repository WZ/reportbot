// Package publish renders the live Weekly Report, stores it as the week's Published Report, and
// keeps a Friday Draft Snapshot. No model is called here.
package publish

import (
	"context"
	"database/sql"
	"log"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"
)

// Live is the current state of one week's report.
type Live struct {
	Monday, NextMonday time.Time
	View               report.View
	Taxonomy           domain.Taxonomy
}

// LoadWeek builds the Weekly Report for the week containing now.
func LoadWeek(db *sql.DB, cfg config.Config, now time.Time) (Live, error) {
	monday, next := domain.ReportWeekRange(cfg, now.In(cfg.Location))
	return LoadRange(db, monday, next)
}

func LoadRange(db *sql.DB, monday, next time.Time) (Live, error) {
	tax, err := sqlite.LoadTaxonomy(db)
	if err != nil {
		return Live{}, err
	}
	items, err := sqlite.GetWeekItems(db, monday, next)
	if err != nil {
		return Live{}, err
	}
	return Live{Monday: monday, NextMonday: next, View: report.BuildView(tax, items), Taxonomy: tax}, nil
}

// Result is a stored Published Report and the files written for it.
type Result struct {
	Live
	Team, Boss         string
	TeamPath, BossPath string
}

// Publish renders team and boss versions, writes the .md and .eml files and stores
// (or replaces) the week's Published Report.
func Publish(db *sql.DB, cfg config.Config, live Live, by string) (Result, error) {
	r := Result{Live: live, Team: report.RenderTeamMarkdown(live.View), Boss: report.RenderBossMarkdown(live.View)}
	friday := domain.FridayOfWeek(live.Monday)
	var err error
	if r.TeamPath, err = report.WriteReportFile(r.Team, cfg.ReportOutputDir, friday, cfg.TeamName); err != nil {
		return r, err
	}
	if r.BossPath, err = report.WriteEmailDraftFile(r.Boss, cfg.ReportOutputDir, friday, cfg.TeamName); err != nil {
		return r, err
	}
	err = sqlite.SavePublishedReport(db, sqlite.PublishedReport{
		WeekStart: live.Monday.Format("2006-01-02"), TeamMarkdown: r.Team, BossMarkdown: r.Boss,
		UndeterminedCount: len(live.View.Undetermined), PublishedBy: by,
	})
	return r, err
}

const draftHour = 9 // the Draft Snapshot is taken on Fridays at 09:00

// NextDraftSnapshotTime is the next Friday 09:00 after now in loc.
func NextDraftSnapshotTime(now time.Time, loc *time.Location) time.Time {
	now = now.In(loc)
	days := (int(time.Friday) - int(now.Weekday()) + 7) % 7
	next := time.Date(now.Year(), now.Month(), now.Day()+days, draftHour, 0, 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 7)
	}
	return next
}

// SaveDraftSnapshot stores the current Weekly Report, rendered as publishing renders it, as the week's Draft Snapshot.
func SaveDraftSnapshot(db *sql.DB, cfg config.Config, now time.Time) error {
	live, err := LoadWeek(db, cfg, now)
	if err != nil {
		return err
	}
	return sqlite.SaveDraftSnapshot(db, sqlite.DraftSnapshot{
		WeekStart: live.Monday.Format("2006-01-02"), TeamMarkdown: report.RenderTeamMarkdown(live.View),
		BossMarkdown: report.RenderBossMarkdown(live.View), ItemCount: live.View.ItemCount(), UndeterminedCount: len(live.View.Undetermined),
	})
}

// StartDraftSnapshotScheduler saves a Draft Snapshot every Friday at 09:00 until ctx ends.
func StartDraftSnapshotScheduler(ctx context.Context, db *sql.DB, cfg config.Config) {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	go func() {
		for {
			next := NextDraftSnapshotTime(time.Now(), loc)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			if err := SaveDraftSnapshot(db, cfg, time.Now()); err != nil {
				log.Printf("Draft Snapshot: %v", err)
			} else {
				log.Printf("Draft Snapshot saved on %s", next.Format("2006-01-02"))
			}
		}
	}()
}
