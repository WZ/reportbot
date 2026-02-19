package llm

import "strings"

func normalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "merged", "shipped":
		return "done"
	case "in test", "in testing", "qa", "in qa", "testing":
		return "in testing"
	case "in progress", "working", "wip", "progress":
		return "in progress"
	default:
		return strings.TrimSpace(status)
	}
}
