package filing

import (
	"regexp"
	"strings"
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

// FindTickets returns tickets that are certain (a leading [..] list, or the only plausible
// number) and, when two or more numbers are plausible, the candidates for the model.
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
	for _, m := range numberRe.FindAllStringSubmatchIndex(text, -1) {
		num := text[m[4]:m[5]]
		// "#123" / "!123" are merge request or issue references; build and version numbers are not tickets.
		if seen[num] || m[3] > m[2] || len(num) < 6 || mentionsBuild(description, num) {
			continue
		}
		seen[num] = true
		lo, hi := max(0, m[2]-24), min(len(description), m[5]+24)
		candidates = append(candidates, TicketCandidate{Number: num, Context: strings.TrimSpace(description[lo:hi])})
	}
	if len(candidates) == 1 {
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
