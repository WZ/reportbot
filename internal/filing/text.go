package filing

import (
	"regexp"
	"strings"

	"reportbot/internal/domain"
)

var statusSuffixRe = regexp.MustCompile(`\(([^()]*)\)\s*$`)

// SplitStatus separates a trailing "(status text)" from a report line.
func SplitStatus(line string) (description, statusText string) {
	line = strings.TrimSpace(line)
	if m := statusSuffixRe.FindStringSubmatchIndex(line); m != nil {
		return strings.TrimSpace(line[:m[0]]), strings.TrimSpace(line[m[2]:m[3]])
	}
	return line, ""
}

var statusWords = map[string]string{
	"done": domain.StatusDone, "completed": domain.StatusDone, "complete": domain.StatusDone, "finished": domain.StatusDone,
	"merged": domain.StatusDone, "shipped": domain.StatusDone, "resolved": domain.StatusDone, "fixed": domain.StatusDone,
	"in testing": domain.StatusInTesting, "in test": domain.StatusInTesting, "testing": domain.StatusInTesting,
	"qa": domain.StatusInTesting, "in qa": domain.StatusInTesting, "verifying": domain.StatusInTesting,
	"in progress": domain.StatusInProgress, "in-progress": domain.StatusInProgress, "wip": domain.StatusInProgress,
	"working": domain.StatusInProgress, "progress": domain.StatusInProgress, "ongoing": domain.StatusInProgress,
	"blocked": domain.StatusBlocked, "on hold": domain.StatusBlocked,
}

// MapStatus maps status text that is just a status word. ok=false means the
// decision model has to decide; the text is then kept as the Status Note.
func MapStatus(text string) (status string, ok bool) {
	s, ok := statusWords[strings.ToLower(strings.Join(strings.Fields(text), " "))]
	return s, ok
}

var (
	leadingTicketsRe = regexp.MustCompile(`^\s*\[([0-9][0-9,\s]*)\]`)
	versionRe        = regexp.MustCompile(`\d+(?:\.\d+)+`)
	numberRe         = regexp.MustCompile(`(?:^|[^\w.#!])([#!]?)(\d{4,})\b`)
)

// TicketCandidate is a bare number that might be a ticket ID, with nearby text.
type TicketCandidate struct {
	Number  string
	Context string
}

// FindTickets returns tickets that are certain (a leading [..] list, or a single
// long number with nothing competing) and, otherwise, candidates for the model.
func FindTickets(description string) (certain []string, candidates []TicketCandidate) {
	if m := leadingTicketsRe.FindStringSubmatch(description); m != nil {
		for _, p := range strings.Split(m[1], ",") {
			if p = strings.TrimSpace(p); p != "" {
				certain = append(certain, p)
			}
		}
		return certain, nil
	}
	text := versionRe.ReplaceAllStringFunc(description, func(v string) string { return strings.Repeat(" ", len(v)) })
	seen := map[string]bool{}
	referenced := false
	for _, m := range numberRe.FindAllStringSubmatchIndex(text, -1) {
		num := text[m[4]:m[5]]
		if seen[num] {
			continue
		}
		seen[num] = true
		// "#123" / "!123" are merge request or issue references.
		referenced = referenced || m[3] > m[2]
		lo, hi := max(0, m[2]-24), min(len(description), m[5]+24)
		candidates = append(candidates, TicketCandidate{Number: num, Context: strings.TrimSpace(description[lo:hi])})
	}
	if len(candidates) == 1 && len(candidates[0].Number) >= 6 && !referenced && !mentionsBuild(description, candidates[0].Number) {
		return []string{candidates[0].Number}, nil
	}
	return nil, candidates
}

func mentionsBuild(description, number string) bool {
	lower := strings.ToLower(description)
	i := strings.Index(lower, number)
	before := strings.TrimSpace(lower[:max(0, i)])
	for _, w := range []string{"build", "b", "mr", "pr", "version", "v"} {
		if strings.HasSuffix(before, w) {
			return true
		}
	}
	return false
}
