package learning

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"reportbot/internal/domain"
)

// Learned Notes reach the decision model's criteria, so a draft must be plain, inert text.
var (
	urlRe          = regexp.MustCompile(`(?i)\b(?:https?|ftp)://|\bwww\.`)
	slackMentionRe = regexp.MustCompile(`<[@#!]`)
	tagRe          = regexp.MustCompile(`<[^>]*>`)
	mdLinkRe       = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	// addressesModel matches text that speaks to the model instead of describing work.
	addressesModel = []string{"ignore", "always choose", "you must", "answer", "system", "instruction"}
	// sectionRefRe finds a Section named as where work goes instead, e.g. "those are Infrastructure".
	sectionRefRe = regexp.MustCompile(`(?i:those are|that is|these are|goes? (?:to|in|under)|belongs? (?:to|in|under)|filed? (?:to|in|under)|see)\s+(?i:the\s+)?([A-Z0-9][\w./&+-]*(?:\s+[A-Z0-9][\w./&+-]*)*)`)
)

// ValidateNote rejects a drafted note unless it is plain text within the cap, does not address
// the model, names only existing Sections (knownNames) and names none of authors.
func ValidateNote(note string, knownNames, authors []string) error {
	n := strings.TrimSpace(note)
	switch {
	case n == "":
		return fmt.Errorf("empty")
	case len([]rune(n)) > domain.MaxLearnedNotes:
		return fmt.Errorf("longer than %d characters", domain.MaxLearnedNotes)
	case urlRe.MatchString(n):
		return fmt.Errorf("contains a URL")
	case slackMentionRe.MatchString(n):
		return fmt.Errorf("contains a Slack mention")
	case strings.Contains(n, "`"):
		return fmt.Errorf("contains code formatting")
	case mdLinkRe.MatchString(n):
		return fmt.Errorf("contains a markdown link")
	case tagRe.MatchString(n):
		return fmt.Errorf("contains markup")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return fmt.Errorf("contains a control character")
		}
	}
	lower := strings.ToLower(n)
	for _, p := range addressesModel {
		if strings.Contains(lower, p) {
			return fmt.Errorf("addresses the model (%q)", p)
		}
	}
	for _, m := range sectionRefRe.FindAllStringSubmatch(n, -1) {
		if !startsWithKnown(m[1], knownNames) {
			return fmt.Errorf("names a Section that does not exist (%q)", m[1])
		}
	}
	for _, a := range authors {
		for _, tok := range strings.FieldsFunc(a, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' && r != '\'' }) {
			if len([]rune(tok)) >= 3 && regexp.MustCompile(`\b`+regexp.QuoteMeta(tok)+`\b`).MatchString(n) {
				return fmt.Errorf("names a person")
			}
		}
	}
	return nil
}

// startsWithKnown reports whether ref begins with an existing Taxonomy node name.
func startsWithKnown(ref string, known []string) bool {
	ref = strings.ToLower(strings.TrimRight(ref, ".,;:"))
	for _, k := range known {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && (ref == k || strings.HasPrefix(ref, k+" ")) {
			return true
		}
	}
	return false
}

// TaxonomyNames are the names a note may use to refer to a Section.
func TaxonomyNames(sections []domain.Section) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sections {
		for _, n := range []string{s.Area, s.Category, s.Name} {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
