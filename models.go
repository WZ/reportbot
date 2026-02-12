package main

import "time"

type WorkItem struct {
	ID          int64
	Description string
	Author      string
	AuthorID    string // Slack user ID (immutable, for authorization)
	Source      string // "slack" or "gitlab"
	SourceRef   string // GitLab MR URL or empty
	Category    string
	Status      string // "done", "in progress", "in QA", etc.
	TicketIDs   string // comma-separated: "1247202,1230118"
	ReportedAt  time.Time
	CreatedAt   time.Time
}

type GitLabMR struct {
	Title       string
	Author      string // username
	AuthorName  string // display name
	WebURL      string
	MergedAt    time.Time
	UpdatedAt   time.Time
	CreatedAt   time.Time
	State       string
	Labels      []string
	ProjectPath string
}

type ReportSection struct {
	Category string
	Authors  []string
	Items    []WorkItem
}

// CurrentWeekRange returns Monday 00:00:00 and next Monday 00:00:00 for the current calendar week.
func CurrentWeekRange(loc *time.Location) (time.Time, time.Time) {
	now := time.Now().In(loc)
	return CurrentWeekRangeAt(now)
}

func CurrentWeekRangeAt(now time.Time) (time.Time, time.Time) {
	weekday := now.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	daysFromMonday := int(weekday) - int(time.Monday)
	monday := time.Date(now.Year(), now.Month(), now.Day()-daysFromMonday, 0, 0, 0, 0, now.Location())
	nextMonday := monday.AddDate(0, 0, 7)
	return monday, nextMonday
}

func ReportWeekRange(cfg Config, now time.Time) (time.Time, time.Time) {
	hour, min, err := parseClock(cfg.MondayCutoffTime)
	if err != nil {
		return CurrentWeekRangeAt(now)
	}

	if now.Weekday() == time.Monday {
		cutoff := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if now.Before(cutoff) {
			return CurrentWeekRangeAt(now.AddDate(0, 0, -7))
		}
	}
	return CurrentWeekRangeAt(now)
}
