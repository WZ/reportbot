package report

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"reportbot/internal/domain"
)

var (
	ticketPrefixRe   = regexp.MustCompile(`^\[([^\]]+)\]\s+`)
	bareTicketLeadRe = regexp.MustCompile(`^\s*(?:#\s*)?(?:[A-Za-z][A-Za-z0-9_]*-)?([0-9A-Za-z]+)(?:\s*[:,-]\s*|\s+)`)
	nameAliasParenRe = regexp.MustCompile(`\([^)]*\)|（[^）]*）`)
)

// GenerationMu prevents concurrent publishing from Slack and the web UI.
var GenerationMu sync.Mutex

// UndeterminedHeading is the heading for Work Items without a Section.
const UndeterminedHeading = "Undetermined"

// View is a Weekly Report organised by the Taxonomy; empty parts are omitted.
type View struct {
	Areas        []AreaView
	Undetermined []WorkItem
}

type AreaView struct {
	Name       string
	Categories []CategoryView
}

type CategoryView struct {
	Name  string
	ID    int64
	Items []WorkItem // the Category is itself the Section
	// Sections under the Category, in Taxonomy order.
	Sections []SectionView
}

type SectionView struct {
	Section domain.Section
	Items   []WorkItem
}

// ItemCount counts every Work Item in the view.
func (v View) ItemCount() int {
	n := len(v.Undetermined)
	for _, a := range v.Areas {
		for _, c := range a.Categories {
			n += len(c.Items)
			for _, s := range c.Sections {
				n += len(s.Items)
			}
		}
	}
	return n
}

// BuildView places Work Items into the active Taxonomy; those without an active Section
// are Undetermined.
func BuildView(tax domain.Taxonomy, items []WorkItem) View {
	bySection := map[int64][]WorkItem{}
	var v View
	active := map[int64]bool{}
	for _, s := range tax.Sections() {
		active[s.ID] = true
	}
	for _, it := range items {
		if it.SectionID != 0 && active[it.SectionID] {
			bySection[it.SectionID] = append(bySection[it.SectionID], it)
		} else {
			v.Undetermined = append(v.Undetermined, it)
		}
	}
	for _, area := range tax.Areas() {
		av := AreaView{Name: area.Name}
		for _, cat := range tax.Children(area.ID) {
			cv := CategoryView{Name: cat.Name, ID: cat.ID, Items: sortItems(bySection[cat.ID])}
			for _, sec := range tax.Children(cat.ID) {
				s, _ := tax.Section(sec.ID)
				if its := bySection[sec.ID]; len(its) > 0 {
					cv.Sections = append(cv.Sections, SectionView{Section: s, Items: sortItems(its)})
				}
			}
			if len(cv.Items) > 0 || len(cv.Sections) > 0 {
				av.Categories = append(av.Categories, cv)
			}
		}
		if len(av.Categories) > 0 {
			v.Areas = append(v.Areas, av)
		}
	}
	v.Undetermined = sortItems(v.Undetermined)
	return v
}

// sortItems orders by Status (in progress, in testing, blocked, done), then report time.
func sortItems(items []WorkItem) []WorkItem {
	out := append([]WorkItem(nil), items...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := domain.StatusRank(out[i].Status), domain.StatusRank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		if !out[i].ReportedAt.Equal(out[j].ReportedAt) {
			return out[i].ReportedAt.Before(out[j].ReportedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RenderTeamMarkdown renders the team version: authors on every line.
func RenderTeamMarkdown(v View) string {
	return renderMarkdown(v, func(c CategoryView) string { return c.Name }, FormatTeamItem)
}

// RenderBossMarkdown renders the boss version: authors in the Category heading.
func RenderBossMarkdown(v View) string {
	return renderMarkdown(v, func(c CategoryView) string {
		return mergeCategoryHeadingAuthors(c.Name, categoryAuthors(c))
	}, formatBossItem)
}

// RenderMarkdownByMode renders "team" or "boss".
func RenderMarkdownByMode(v View, mode string) string {
	if mode == "boss" {
		return RenderBossMarkdown(v)
	}
	return RenderTeamMarkdown(v)
}

func renderMarkdown(v View, heading func(CategoryView) string, format func(WorkItem) string) string {
	var buf strings.Builder
	for _, a := range v.Areas {
		buf.WriteString("### " + a.Name + "\n\n")
		for _, c := range a.Categories {
			buf.WriteString("#### " + heading(c) + "\n\n")
			for _, it := range c.Items {
				buf.WriteString("- " + format(it) + "\n")
			}
			if len(c.Items) > 0 {
				buf.WriteString("\n")
			}
			for _, s := range c.Sections {
				buf.WriteString("- **" + s.Section.Name + "**\n")
				for _, it := range s.Items {
					buf.WriteString("  - " + format(it) + "\n")
				}
				buf.WriteString("\n")
			}
		}
	}
	if len(v.Undetermined) > 0 {
		buf.WriteString("#### " + UndeterminedHeading + "\n\n")
		for _, it := range v.Undetermined {
			buf.WriteString("- " + format(it) + "\n")
		}
	}
	return strings.TrimSpace(buf.String()) + "\n"
}

func categoryAuthors(c CategoryView) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(items []WorkItem) {
		for _, it := range items {
			author := synthesizeName(it.Author)
			if author != "" && !seen[author] {
				seen[author] = true
				out = append(out, author)
			}
		}
	}
	add(c.Items)
	for _, s := range c.Sections {
		add(s.Items)
	}
	return out
}

// StatusText is the "(…)" part of a line: the Status, plus the Status Note when there is one.
func StatusText(it WorkItem) string {
	status := it.Status
	if status == "" {
		status = "filing…"
	}
	if note := strings.TrimSpace(it.StatusNote); note != "" && !strings.EqualFold(note, status) {
		return status + ": " + note
	}
	return status
}

// FormatTeamItem is the code template "**Author** - [tickets] description (status)".
func FormatTeamItem(it WorkItem) string {
	author := synthesizeName(it.Author)
	body := formatBossItem(it)
	if author == "" {
		return body
	}
	return fmt.Sprintf("**%s** - %s", author, body)
}

func formatBossItem(it WorkItem) string {
	tickets := canonicalTicketIDs(it.TicketIDs)
	description := synthesizeDescription(stripLeadingTicketPrefixIfSame(it.Description, tickets))
	ticketPrefix := ""
	if tickets != "" {
		ticketPrefix = fmt.Sprintf("[%s] ", tickets)
	}
	return fmt.Sprintf("%s%s (%s)", ticketPrefix, description, StatusText(it))
}

// SynthesizeName title-cases a person's name and drops aliases in parentheses.
func SynthesizeName(name string) string { return synthesizeName(name) }

func canonicalTicketIDs(ticketIDs string) string {
	if strings.TrimSpace(ticketIDs) == "" {
		return ""
	}
	parts := strings.Split(ticketIDs, ",")
	cleaned := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = normalizeTicketTokenForOutput(p)
		if p == "" {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, p)
	}
	return strings.Join(cleaned, ",")
}

func normalizeTicketTokenForOutput(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	t = strings.Trim(t, "[]")
	t = strings.TrimSpace(t)
	t = strings.TrimLeft(t, "#")
	return strings.TrimSpace(t)
}

func stripLeadingTicketPrefixIfSame(description, tickets string) string {
	description = strings.TrimSpace(description)
	if description == "" || tickets == "" {
		return description
	}
	ticketSet := make(map[string]bool)
	for _, t := range strings.Split(tickets, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		ticketSet[strings.ToLower(t)] = true
	}

	for {
		trimmed := false
		matches := ticketPrefixRe.FindStringSubmatch(description)
		if len(matches) == 2 {
			leading := canonicalTicketIDs(matches[1])
			if leading == tickets {
				description = strings.TrimSpace(description[len(matches[0]):])
				trimmed = true
			}
		}
		if trimmed {
			continue
		}

		// Handle plain leading ticket mentions like:
		// "1234567 Implement ...", "#1234567 Implement ...", "JIRA-1234567 Implement ..."
		bare := bareTicketLeadRe.FindStringSubmatchIndex(description)
		if len(bare) == 4 {
			candidate := strings.ToLower(strings.TrimSpace(description[bare[2]:bare[3]]))
			if ticketSet[candidate] {
				description = strings.TrimSpace(description[bare[1]:])
				continue
			}
		}

		break
	}
	return description
}

func synthesizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = nameAliasParenRe.ReplaceAllString(name, " ")
	parts := strings.Fields(strings.ToLower(name))
	for i, p := range parts {
		runes := []rune(p)
		for j, r := range runes {
			if unicode.IsLetter(r) {
				runes[j] = unicode.ToUpper(r)
				break
			}
		}
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}

func synthesizeDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	runes := []rune(description)
	for i, r := range runes {
		if unicode.IsLetter(r) {
			runes[i] = unicode.ToUpper(r)
			break
		}
	}
	return string(runes)
}

func mergeCategoryHeadingAuthors(categoryName string, generatedAuthors []string) string {
	base, existingAuthors := splitCategoryNameAndAuthors(categoryName)
	merged := mergeAuthors(generatedAuthors, existingAuthors)
	if len(merged) == 0 {
		return strings.TrimSpace(base)
	}
	return fmt.Sprintf("%s (%s)", strings.TrimSpace(base), strings.Join(merged, ", "))
}

func splitCategoryNameAndAuthors(categoryName string) (string, []string) {
	s := strings.TrimSpace(categoryName)
	var groups [][]string
	for {
		base, inside, ok := popTrailingParenGroup(s)
		if !ok {
			break
		}
		inside = strings.TrimSpace(inside)
		if inside == "" || !looksLikeAuthorGroup(inside) {
			break
		}
		var names []string
		for _, p := range strings.Split(inside, ",") {
			name := synthesizeName(strings.TrimSpace(p))
			if name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			groups = append([][]string{names}, groups...)
		}
		s = strings.TrimSpace(base)
	}
	var existing []string
	for _, g := range groups {
		existing = append(existing, g...)
	}
	return s, existing
}

func popTrailingParenGroup(s string) (base string, inside string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	runes := []rune(s)
	end := len(runes) - 1
	if runes[end] != ')' {
		return "", "", false
	}
	depth := 0
	start := -1
	for i := end; i >= 0; i-- {
		switch runes[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				start = i
				i = -1
			}
		}
	}
	if start < 0 || start >= end {
		return "", "", false
	}
	base = strings.TrimSpace(string(runes[:start]))
	inside = string(runes[start+1 : end])
	return base, inside, true
}

func looksLikeAuthorGroup(group string) bool {
	if strings.TrimSpace(group) == "" {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(group))
	statusWords := []string{"in progress", "in testing", "in test", "done", "qa"}
	for _, w := range statusWords {
		if strings.Contains(lower, w) {
			return false
		}
	}
	return true
}

func mergeAuthors(primary []string, secondary []string) []string {
	var out []string
	add := func(name string) {
		name = synthesizeName(name)
		if name == "" {
			return
		}
		for i, existing := range out {
			if personNameEquivalent(existing, name) {
				if len(strings.Fields(name)) > len(strings.Fields(existing)) {
					out[i] = name
				}
				return
			}
		}
		out = append(out, name)
	}
	for _, n := range primary {
		add(n)
	}
	for _, n := range secondary {
		add(n)
	}
	return out
}

func personNameEquivalent(a, b string) bool {
	ta := nameTokens(a)
	tb := nameTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	return tokenSubset(ta, tb) || tokenSubset(tb, ta)
}

func nameTokens(s string) []string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	replaced := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return ' '
	}, s)
	return strings.Fields(replaced)
}

func tokenSubset(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, t := range b {
		set[t] = struct{}{}
	}
	for _, t := range a {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}
