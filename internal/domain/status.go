package domain

import "strings"

var statusWords = map[string]string{
	"done": StatusDone, "completed": StatusDone, "complete": StatusDone, "finished": StatusDone,
	"merged": StatusDone, "shipped": StatusDone, "resolved": StatusDone, "fixed": StatusDone,
	"in testing": StatusInTesting, "in test": StatusInTesting, "testing": StatusInTesting,
	"qa": StatusInTesting, "in qa": StatusInTesting, "verifying": StatusInTesting,
	"in progress": StatusInProgress, "in-progress": StatusInProgress, "wip": StatusInProgress,
	"working": StatusInProgress, "progress": StatusInProgress, "ongoing": StatusInProgress,
	"blocked": StatusBlocked, "on hold": StatusBlocked,
}

// StatusFromWord maps status text that is just a status word. ok=false means the text
// says more than a status (filing asks the decision model; the text is kept as the Status Note).
func StatusFromWord(text string) (status string, ok bool) {
	s, ok := statusWords[strings.ToLower(strings.Join(strings.Fields(text), " "))]
	return s, ok
}

// GuessStatus maps any status text by keyword, for places that cannot ask a model
// (the one-off cut-over). Empty text means done, as in the reports it reads.
func GuessStatus(text string) string {
	if s, ok := StatusFromWord(text); ok {
		return s
	}
	t := strings.ToLower(text)
	switch {
	case strings.TrimSpace(t) == "":
		return StatusDone
	case strings.Contains(t, "block"):
		return StatusBlocked
	case strings.Contains(t, "review"), strings.Contains(t, "test"), strings.Contains(t, "verif"):
		return StatusInTesting
	case strings.Contains(t, "resolved"), strings.Contains(t, "reverted"), strings.Contains(t, "fixed"),
		strings.Contains(t, "done"), strings.Contains(t, "complete"), strings.Contains(t, "merged"):
		return StatusDone
	default:
		return StatusInProgress
	}
}
