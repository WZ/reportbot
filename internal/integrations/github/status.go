package github

import "time"

func mapPRStatus(pr GitHubPR) string {
	if pr.State == "merged" {
		return "done"
	}
	if pr.State == "open" {
		return "in progress"
	}
	return "done"
}

func prReportedAt(pr GitHubPR, loc *time.Location) time.Time {
	if pr.State == "open" && !pr.UpdatedAt.IsZero() {
		return pr.UpdatedAt
	}
	if !pr.MergedAt.IsZero() {
		return pr.MergedAt
	}
	if !pr.CreatedAt.IsZero() {
		return pr.CreatedAt
	}
	return time.Now().In(loc)
}
