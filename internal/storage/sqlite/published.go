package sqlite

import (
	"database/sql"
	"time"
)

type PublishedReport struct {
	WeekStart         string // Monday, YYYY-MM-DD
	TeamMarkdown      string
	BossMarkdown      string
	UndeterminedCount int
	PublishedBy       string
	PublishedAt       time.Time
}

// SavePublishedReport stores the week's Published Report, replacing an earlier one.
func SavePublishedReport(db *sql.DB, r PublishedReport) error {
	_, err := db.Exec(
		`INSERT INTO published_reports (week_start, team_md, boss_md, undetermined_count, published_by, published_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(week_start) DO UPDATE SET team_md = excluded.team_md, boss_md = excluded.boss_md,
		   undetermined_count = excluded.undetermined_count, published_by = excluded.published_by, published_at = excluded.published_at`,
		r.WeekStart, r.TeamMarkdown, r.BossMarkdown, r.UndeterminedCount, r.PublishedBy, time.Now(),
	)
	return err
}

func GetPublishedReport(db *sql.DB, weekStart string) (PublishedReport, bool, error) {
	var r PublishedReport
	err := db.QueryRow(
		`SELECT week_start, team_md, boss_md, undetermined_count, published_by, published_at FROM published_reports WHERE week_start = ?`, weekStart,
	).Scan(&r.WeekStart, &r.TeamMarkdown, &r.BossMarkdown, &r.UndeterminedCount, &r.PublishedBy, &r.PublishedAt)
	if err == sql.ErrNoRows {
		return r, false, nil
	}
	return r, err == nil, err
}

func ListPublishedWeeks(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT week_start FROM published_reports ORDER BY week_start DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
