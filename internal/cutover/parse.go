// Package cutover is the one-off move from markdown-derived reports to the database
// Taxonomy: it seeds the Taxonomy and Work Items from published reports and drops V1 tables.
package cutover

import (
	"regexp"
	"strings"

	"reportbot/internal/domain"
	"reportbot/internal/filing"
)

// Line is an authored item line of a published report.
type Line struct {
	Area, Category, Section string // Section empty: the line sits at Category level
	Author                  string
	Description             string // without the trailing (status)
	StatusText              string
	TicketIDs               string
	Undetermined            bool
}

// Report is a parsed published report.
type Report struct {
	Areas []AreaHeading
	Lines []Line
}

type AreaHeading struct {
	Name       string
	Categories []CategoryHeading
}

type CategoryHeading struct {
	Name     string
	Sections []string
}

var (
	areaRe      = regexp.MustCompile(`^###\s+(.+?)\s*$`)
	categoryRe  = regexp.MustCompile(`^####\s+(.+?)\s*$`)
	sectionRe   = regexp.MustCompile(`^[-*]\s+\*\*([^*]+)\*\*\s*((?:\([^()]*\)\s*)*)$`)
	itemRe      = regexp.MustCompile(`^(\s*)[-*]\s+\*\*([^*]+?)\s*-?\*\*\s*-\s*(.+)$`)
	itemNoDash  = regexp.MustCompile(`^(\s*)[-*]\s+\*\*([^*]+?)\s*-\*\*\s*(.+)$`)
	leadTickets = regexp.MustCompile(`^\[([0-9][0-9,\s]*)\]\s*`)
)

// Parse reads "### Area", "#### Category", "- **Section**" headings and
// "- **Author** - [tickets] description (status)" lines, with "-" or "*" bullets.
// Lines without a bold author (plan outlines) are skipped.
func Parse(markdown string) Report {
	var r Report
	var area, category, section string
	inUndetermined := false
	ai, ci := -1, -1
	for _, raw := range strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case categoryRe.MatchString(trimmed):
			category = categoryRe.FindStringSubmatch(trimmed)[1]
			section = ""
			inUndetermined = strings.EqualFold(category, "Undetermined")
			if !inUndetermined && ai >= 0 {
				r.Areas[ai].Categories = append(r.Areas[ai].Categories, CategoryHeading{Name: category})
				ci = len(r.Areas[ai].Categories) - 1
			}
			continue
		case areaRe.MatchString(trimmed):
			area = areaRe.FindStringSubmatch(trimmed)[1]
			category, section, inUndetermined = "", "", false
			r.Areas = append(r.Areas, AreaHeading{Name: area})
			ai, ci = len(r.Areas)-1, -1
			continue
		}
		indented := line != strings.TrimLeft(line, " \t")
		if m := sectionRe.FindStringSubmatch(trimmed); m != nil && !indented {
			section = strings.TrimSpace(m[1] + " " + strings.TrimSpace(m[2]))
			if !inUndetermined && ci >= 0 {
				c := &r.Areas[ai].Categories[ci]
				c.Sections = append(c.Sections, section)
			}
			continue
		}
		m := itemRe.FindStringSubmatch(line)
		if m == nil {
			m = itemNoDash.FindStringSubmatch(line)
		}
		if m == nil || category == "" {
			continue
		}
		l := Line{Area: area, Category: category, Author: strings.TrimSpace(m[2]), Undetermined: inUndetermined}
		if m[1] != "" {
			l.Section = section
		}
		l.Description, l.StatusText = filing.SplitStatus(m[3])
		if t := leadTickets.FindStringSubmatch(l.Description); t != nil {
			var ids []string
			for _, p := range strings.Split(t[1], ",") {
				if p = strings.TrimSpace(p); p != "" {
					ids = append(ids, p)
				}
			}
			l.TicketIDs = strings.Join(ids, ",")
		}
		r.Lines = append(r.Lines, l)
	}
	return r
}

// LineStatus maps a line's status text; free text is kept as the Status Note.
func LineStatus(text string) (status, note string) {
	if s, ok := filing.MapStatus(text); ok {
		return s, ""
	}
	return RuleStatus(text), strings.TrimSpace(text)
}

// RuleStatus maps free-text V1 statuses without a model, for the one-off migration.
func RuleStatus(text string) string {
	if s, ok := filing.MapStatus(text); ok {
		return s
	}
	t := strings.ToLower(text)
	switch {
	case strings.TrimSpace(t) == "":
		return domain.StatusDone
	case strings.Contains(t, "block"):
		return domain.StatusBlocked
	case strings.Contains(t, "review"), strings.Contains(t, "test"), strings.Contains(t, "verif"):
		return domain.StatusInTesting
	case strings.Contains(t, "resolved"), strings.Contains(t, "reverted"), strings.Contains(t, "fixed"),
		strings.Contains(t, "done"), strings.Contains(t, "complete"), strings.Contains(t, "merged"):
		return domain.StatusDone
	default:
		return domain.StatusInProgress
	}
}

var (
	normTicketRe = regexp.MustCompile(`\[[\d,\s]+\]`)
	normParenRe  = regexp.MustCompile(`\s*\([^()]*\)\s*$`)
	normNonWord  = regexp.MustCompile(`[^a-z0-9]+`)
)

// NormalizeDescription matches DB text to report lines: lower-case, ticket brackets
// and a trailing (status) removed, punctuation collapsed.
func NormalizeDescription(s string) string {
	s = strings.ToLower(s)
	s = normTicketRe.ReplaceAllString(s, "")
	s = normParenRe.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.TrimSpace(normNonWord.ReplaceAllString(s, " "))
}
