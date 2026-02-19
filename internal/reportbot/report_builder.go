package reportbot

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type ReportTemplate struct {
	PrefixLines []string
	Categories  []TemplateCategory
}

type TemplateCategory struct {
	Name        string
	Subsections []TemplateSubsection
	MarkerLine  string
}

type TemplateSubsection struct {
	Name       string
	HeaderLine string
	Items      []TemplateItem
}

type TemplateItem struct {
	Author      string
	Description string
	TicketIDs   string
	Status      string
	IsNew       bool
	ReportedAt  time.Time
}

type sectionOption struct {
	ID         string
	Category   int
	Subsection int
	Label      string
}

type loadStatus int

const (
	templateFromFile loadStatus = iota
	templateFirstEver
)

var (
	categoryHeadingRe = regexp.MustCompile(`^\s*####\s+(.+?)\s*$`)
	topHeadingRe      = regexp.MustCompile(`^\s*###\s+(.+?)\s*$`)
	subcategoryRe     = regexp.MustCompile(`^\s*-\s+\*\*(.+?)\*\*(.*)$`)
	bulletLineRe      = regexp.MustCompile(`^\s*-\s+(.+?)\s*$`)
	statusSuffixRe    = regexp.MustCompile(`\(([^)]+)\)\s*$`)
	ticketPrefixRe    = regexp.MustCompile(`^\[([^\]]+)\]\s+`)
	authorPrefixRe    = regexp.MustCompile(`^\*\*(.+?)\*\*\s*-\s*`)
	nameAliasParenRe  = regexp.MustCompile(`\([^)]*\)|（[^）]*）`)
)

var classifySectionsFn = func(cfg Config, items []WorkItem, options []sectionOption, existing []existingItemContext, corrections []ClassificationCorrection, historicalItems []historicalItem) (map[int64]LLMSectionDecision, LLMUsage, error) {
	return CategorizeItemsToSections(cfg, items, options, existing, corrections, historicalItems)
}

type BuildResult struct {
	Template  *ReportTemplate
	Usage     LLMUsage
	Decisions map[int64]LLMSectionDecision
	Options   []sectionOption
}

func BuildReportsFromLast(cfg Config, items []WorkItem, reportDate time.Time, corrections []ClassificationCorrection, historicalItems []historicalItem) (BuildResult, error) {
	template, status, err := loadTemplateForGeneration(cfg.ReportOutputDir, cfg.TeamName, reportDate)
	if err != nil {
		return BuildResult{}, err
	}
	stripCurrentTeamTitleFromPrefix(template, cfg.TeamName)

	merged := cloneTemplate(template)
	trimDoneItems(merged)

	options := templateOptions(template)
	var decisions map[int64]LLMSectionDecision
	llmUsage := LLMUsage{}
	if len(options) > 0 && status != templateFirstEver {
		existing := buildExistingItemContext(merged, options)
		decisions, llmUsage, err = classifySectionsFn(cfg, items, options, existing, corrections, historicalItems)
		if err != nil {
			return BuildResult{Usage: llmUsage}, err
		}
	}

	confidenceThreshold := cfg.LLMConfidence
	if confidenceThreshold <= 0 || confidenceThreshold > 1 {
		confidenceThreshold = 0.70
	}

	mergeIncomingItems(merged, items, options, decisions, confidenceThreshold)
	reorderTemplateItems(merged)

	return BuildResult{
		Template:  merged,
		Usage:     llmUsage,
		Decisions: decisions,
		Options:   options,
	}, nil
}

func loadTemplateForGeneration(outputDir, teamName string, reportDate time.Time) (*ReportTemplate, loadStatus, error) {
	lastPath, err := findLatestReportBefore(outputDir, teamName, reportDate)
	if err != nil {
		if strings.Contains(err.Error(), "no prior report found") {
			return &ReportTemplate{
				Categories: []TemplateCategory{
					{
						Name: "Undetermined",
						Subsections: []TemplateSubsection{
							{},
						},
					},
				},
			}, templateFirstEver, nil
		}
		return nil, templateFromFile, err
	}
	content, err := os.ReadFile(lastPath)
	if err != nil {
		return nil, templateFromFile, fmt.Errorf("reading last report: %w", err)
	}
	template := parseTemplate(string(content))
	if len(template.Categories) == 0 {
		return nil, templateFromFile, fmt.Errorf("last report has no categories: %s", lastPath)
	}
	return template, templateFromFile, nil
}

func findLatestReportBefore(outputDir, teamName string, reportDate time.Time) (string, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", fmt.Errorf("reading report output dir: %w", err)
	}

	sanitized := sanitizeFilename(teamName)
	prefix := sanitized + "_"
	friday := FridayOfWeek(reportDate)
	currentFile := fmt.Sprintf("%s_%s.md", sanitized, friday.Format("20060102"))
	type candidate struct {
		path string
		date time.Time
	}
	var candidates []candidate

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == currentFile || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".md")
		d, err := time.Parse("20060102", raw)
		if err != nil {
			continue
		}
		if !d.Before(reportDate) {
			continue
		}
		candidates = append(candidates, candidate{
			path: filepath.Join(outputDir, name),
			date: d,
		})
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no prior report found in %s for team %s before %s", outputDir, teamName, reportDate.Format("20060102"))
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].date.After(candidates[j].date)
	})
	return candidates[0].path, nil
}

func parseTemplate(content string) *ReportTemplate {
	template := &ReportTemplate{}
	lines := strings.Split(content, "\n")

	currentCat := -1
	currentSub := -1
	seenFirstCategory := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !seenFirstCategory {
			template.PrefixLines = append(template.PrefixLines, line)
		}
		if trimmed == "" {
			continue
		}

		// Preserve mid-report top headings (### ...), e.g. "### Product Beta",
		// by storing them as marker categories in-place.
		if m := topHeadingRe.FindStringSubmatch(line); len(m) == 2 && currentCat >= 0 {
			template.Categories = append(template.Categories, TemplateCategory{
				MarkerLine: "### " + strings.TrimSpace(m[1]),
			})
			currentCat = -1
			currentSub = -1
			continue
		}

		if m := categoryHeadingRe.FindStringSubmatch(line); len(m) == 2 {
			if !seenFirstCategory && len(template.PrefixLines) > 0 {
				template.PrefixLines = template.PrefixLines[:len(template.PrefixLines)-1]
			}
			seenFirstCategory = true
			template.Categories = append(template.Categories, TemplateCategory{Name: strings.TrimSpace(m[1])})
			currentCat = len(template.Categories) - 1
			currentSub = -1
			continue
		}
		if currentCat < 0 {
			continue
		}

		if m := subcategoryRe.FindStringSubmatch(line); len(m) == 3 {
			rest := strings.TrimSpace(m[2])
			// Team-mode item lines look like: - **Author** - Description (status).
			// Do not treat those as subsection headers.
			if strings.HasPrefix(rest, "-") {
				goto itemLine
			}
			sub := TemplateSubsection{
				Name:       strings.TrimSpace(m[1]),
				HeaderLine: strings.TrimSpace(line),
			}
			template.Categories[currentCat].Subsections = append(template.Categories[currentCat].Subsections, sub)
			currentSub = len(template.Categories[currentCat].Subsections) - 1
			continue
		}

	itemLine:
		if m := bulletLineRe.FindStringSubmatch(line); len(m) == 2 {
			if currentSub < 0 {
				template.Categories[currentCat].Subsections = append(template.Categories[currentCat].Subsections, TemplateSubsection{})
				currentSub = len(template.Categories[currentCat].Subsections) - 1
			}
			item := parseTemplateItem(m[1])
			template.Categories[currentCat].Subsections[currentSub].Items = append(template.Categories[currentCat].Subsections[currentSub].Items, item)
		}
	}

	return template
}

func parseTemplateItem(s string) TemplateItem {
	text := strings.TrimSpace(s)
	author := ""
	if m := authorPrefixRe.FindStringSubmatch(text); len(m) == 2 {
		author = strings.TrimSpace(m[1])
		text = strings.TrimSpace(text[len(m[0]):])
	}

	status := ""
	if m := statusSuffixRe.FindStringSubmatch(text); len(m) == 2 {
		status = normalizeStatus(m[1])
		text = strings.TrimSpace(text[:len(text)-len(m[0])])
	}

	ticketIDs := ""
	if m := ticketPrefixRe.FindStringSubmatch(text); len(m) == 2 {
		ticketIDs = strings.TrimSpace(m[1])
		text = strings.TrimSpace(text[len(m[0]):])
	}

	return TemplateItem{
		Author:      author,
		Description: text,
		TicketIDs:   ticketIDs,
		Status:      status,
	}
}

func templateOptions(t *ReportTemplate) []sectionOption {
	var options []sectionOption
	for ci, cat := range t.Categories {
		for si, sub := range cat.Subsections {
			label := cat.Name
			if strings.TrimSpace(sub.Name) != "" {
				label = cat.Name + " > " + sub.Name
			}
			options = append(options, sectionOption{
				ID:         fmt.Sprintf("S%d_%d", ci, si),
				Category:   ci,
				Subsection: si,
				Label:      label,
			})
		}
	}
	return options
}

func cloneTemplate(src *ReportTemplate) *ReportTemplate {
	out := &ReportTemplate{
		PrefixLines: append([]string(nil), src.PrefixLines...),
		Categories:  make([]TemplateCategory, len(src.Categories)),
	}
	for i, cat := range src.Categories {
		out.Categories[i].Name = cat.Name
		out.Categories[i].MarkerLine = cat.MarkerLine
		out.Categories[i].Subsections = make([]TemplateSubsection, len(cat.Subsections))
		for j, sub := range cat.Subsections {
			out.Categories[i].Subsections[j].Name = sub.Name
			out.Categories[i].Subsections[j].HeaderLine = sub.HeaderLine
			out.Categories[i].Subsections[j].Items = append([]TemplateItem(nil), sub.Items...)
		}
	}
	return out
}

func trimDoneItems(t *ReportTemplate) {
	for ci := range t.Categories {
		for si := range t.Categories[ci].Subsections {
			var filtered []TemplateItem
			for _, item := range t.Categories[ci].Subsections[si].Items {
				if statusBucket(item.Status) == 0 {
					continue
				}
				filtered = append(filtered, item)
			}
			t.Categories[ci].Subsections[si].Items = filtered
		}
	}
}

func mergeIncomingItems(
	t *ReportTemplate,
	items []WorkItem,
	options []sectionOption,
	decisions map[int64]LLMSectionDecision,
	confidenceThreshold float64,
) {
	optionByID := make(map[string]sectionOption, len(options))
	for _, option := range options {
		optionByID[option.ID] = option
	}

	undetermined, undeterminedMap := ensureUndeterminedSection(t)
	itemByDupKey := buildDuplicateKeyIndex(t, options)

	for _, item := range items {
		decision := decisions[item.ID]
		useLLM := decision.Confidence >= confidenceThreshold
		status := chooseNormalizedStatus(item.Status, decision.NormalizedStatus, useLLM)
		tickets := strings.TrimSpace(item.TicketIDs)
		if useLLM && strings.TrimSpace(decision.TicketIDs) != "" {
			tickets = strings.TrimSpace(decision.TicketIDs)
		}
		newItem := TemplateItem{
			Author:      strings.TrimSpace(item.Author),
			Description: strings.TrimSpace(item.Description),
			TicketIDs:   tickets,
			Status:      status,
			IsNew:       true,
			ReportedAt:  item.ReportedAt,
		}

		if useLLM {
			if target, ok := itemByDupKey[decision.DuplicateOf]; ok {
				(*target.items)[target.index] = mergeExistingItem((*target.items)[target.index], newItem)
				continue
			}
		}

		sectionID := "UND"
		if useLLM && strings.TrimSpace(decision.SectionID) != "" {
			sectionID = strings.TrimSpace(decision.SectionID)
		}
		option, ok := optionByID[sectionID]
		if !ok {
			key := itemIdentityKey(newItem)
			if idx, exists := undeterminedMap[key]; exists {
				undetermined.Items[idx] = mergeExistingItem(undetermined.Items[idx], newItem)
				continue
			}
			undeterminedMap[key] = len(undetermined.Items)
			undetermined.Items = append(undetermined.Items, newItem)
			continue
		}

		itemsPtr := &t.Categories[option.Category].Subsections[option.Subsection].Items
		found := -1
		for idx := range *itemsPtr {
			if itemIdentityKey((*itemsPtr)[idx]) == itemIdentityKey(newItem) {
				found = idx
				break
			}
		}
		if found >= 0 {
			(*itemsPtr)[found] = mergeExistingItem((*itemsPtr)[found], newItem)
			continue
		}
		*itemsPtr = append(*itemsPtr, newItem)
	}
}

func chooseNormalizedStatus(incomingStatus, llmStatus string, useLLM bool) string {
	if useLLM {
		switch normalizeStatus(llmStatus) {
		case "done", "in testing", "in progress":
			return normalizeStatus(llmStatus)
		}
	}
	return normalizeStatus(incomingStatus)
}

type dupTarget struct {
	items *[]TemplateItem
	index int
}

func buildDuplicateKeyIndex(t *ReportTemplate, options []sectionOption) map[string]dupTarget {
	optionByPos := make(map[[2]int]string, len(options))
	for _, option := range options {
		optionByPos[[2]int{option.Category, option.Subsection}] = option.ID
	}

	out := make(map[string]dupTarget)
	serial := 1
	for ci := range t.Categories {
		for si := range t.Categories[ci].Subsections {
			if _, ok := optionByPos[[2]int{ci, si}]; !ok {
				continue
			}
			items := &t.Categories[ci].Subsections[si].Items
			for ii := range *items {
				key := fmt.Sprintf("K%d", serial)
				out[key] = dupTarget{
					items: items,
					index: ii,
				}
				serial++
			}
		}
	}
	return out
}

func buildExistingItemContext(t *ReportTemplate, options []sectionOption) []existingItemContext {
	optionByPos := make(map[[2]int]string, len(options))
	for _, option := range options {
		optionByPos[[2]int{option.Category, option.Subsection}] = option.ID
	}

	var out []existingItemContext
	serial := 1
	for ci := range t.Categories {
		for si := range t.Categories[ci].Subsections {
			sectionID, ok := optionByPos[[2]int{ci, si}]
			if !ok {
				continue
			}
			for _, item := range t.Categories[ci].Subsections[si].Items {
				out = append(out, existingItemContext{
					Key:         fmt.Sprintf("K%d", serial),
					SectionID:   sectionID,
					Description: item.Description,
					Status:      item.Status,
				})
				serial++
			}
		}
	}
	return out
}

func ensureUndeterminedSection(t *ReportTemplate) (*TemplateSubsection, map[string]int) {
	for ci := range t.Categories {
		if strings.EqualFold(strings.TrimSpace(t.Categories[ci].Name), "Undetermined") {
			if len(t.Categories[ci].Subsections) == 0 {
				t.Categories[ci].Subsections = append(t.Categories[ci].Subsections, TemplateSubsection{})
			}
			return &t.Categories[ci].Subsections[0], buildItemIndex(t.Categories[ci].Subsections[0].Items)
		}
	}

	t.Categories = append(t.Categories, TemplateCategory{
		Name: "Undetermined",
		Subsections: []TemplateSubsection{
			{},
		},
	})
	last := &t.Categories[len(t.Categories)-1].Subsections[0]
	return last, make(map[string]int)
}

func buildItemIndex(items []TemplateItem) map[string]int {
	index := make(map[string]int)
	for i, item := range items {
		index[itemIdentityKey(item)] = i
	}
	return index
}

func mergeExistingItem(existing, incoming TemplateItem) TemplateItem {
	existing.Status = incoming.Status
	existing.TicketIDs = incoming.TicketIDs
	existing.Description = incoming.Description
	if strings.TrimSpace(existing.Author) == "" {
		existing.Author = incoming.Author
	}
	return existing
}

func reorderTemplateItems(t *ReportTemplate) {
	for ci := range t.Categories {
		for si := range t.Categories[ci].Subsections {
			t.Categories[ci].Subsections[si].Items = reorderItems(t.Categories[ci].Subsections[si].Items)
		}
	}
}

func reorderItems(items []TemplateItem) []TemplateItem {
	sorted := make([]TemplateItem, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		bi, bj := statusBucket(sorted[i].Status), statusBucket(sorted[j].Status)
		if bi != bj {
			return bi < bj
		}
		// Within same status bucket: items without timestamp (carried over) first,
		// then sort by reported_at ascending (newly added at the bottom).
		zi, zj := sorted[i].ReportedAt.IsZero(), sorted[j].ReportedAt.IsZero()
		if zi != zj {
			return zi
		}
		return sorted[i].ReportedAt.Before(sorted[j].ReportedAt)
	})
	return sorted
}

func itemIdentityKey(item TemplateItem) string {
	return strings.ToLower(strings.TrimSpace(item.Description))
}

func renderTeamMarkdown(t *ReportTemplate) string {
	return renderMarkdown(
		t,
		func(cat TemplateCategory) string { return cat.Name },
		formatTeamItem,
	)
}

func renderBossMarkdown(t *ReportTemplate) string {
	return renderMarkdown(
		t,
		func(cat TemplateCategory) string {
			return mergeCategoryHeadingAuthors(cat.Name, categoryAuthors(cat))
		},
		formatBossItem,
	)
}

func renderMarkdown(
	t *ReportTemplate,
	categoryHeading func(cat TemplateCategory) string,
	formatItem func(item TemplateItem) string,
) string {
	var buf strings.Builder
	if prefix := strings.TrimSpace(strings.Join(t.PrefixLines, "\n")); prefix != "" {
		buf.WriteString(prefix)
		buf.WriteString("\n\n")
	}

	for _, cat := range t.Categories {
		if strings.TrimSpace(cat.MarkerLine) != "" {
			buf.WriteString(strings.TrimSpace(cat.MarkerLine) + "\n\n")
			continue
		}
		if len(cat.Subsections) == 0 {
			continue
		}
		// Skip categories that have no items in any subsection
		if !categoryHasItems(cat) {
			continue
		}
		buf.WriteString(fmt.Sprintf("#### %s\n\n", categoryHeading(cat)))
		for _, sub := range cat.Subsections {
			if strings.TrimSpace(sub.HeaderLine) != "" {
				buf.WriteString(strings.TrimSpace(sub.HeaderLine) + "\n")
			}
			for _, item := range sub.Items {
				prefix := "- "
				if strings.TrimSpace(sub.HeaderLine) != "" {
					prefix = "  - "
				}
				buf.WriteString(prefix + formatItem(item) + "\n")
			}
			buf.WriteString("\n")
		}
	}

	return strings.TrimSpace(buf.String()) + "\n"
}

func categoryHasItems(cat TemplateCategory) bool {
	for _, sub := range cat.Subsections {
		if len(sub.Items) > 0 {
			return true
		}
	}
	return false
}

func categoryAuthors(cat TemplateCategory) []string {
	seen := make(map[string]bool)
	var out []string
	for _, sub := range cat.Subsections {
		for _, item := range sub.Items {
			author := synthesizeName(item.Author)
			if author == "" || seen[author] {
				continue
			}
			seen[author] = true
			out = append(out, author)
		}
	}
	return out
}

func formatTeamItem(item TemplateItem) string {
	status := normalizeStatus(item.Status)
	if status == "" {
		status = "done"
	}
	author := synthesizeName(item.Author)
	description := synthesizeDescription(item.Description)
	ticketPrefix := ""
	if strings.TrimSpace(item.TicketIDs) != "" {
		ticketPrefix = fmt.Sprintf("[%s] ", strings.TrimSpace(item.TicketIDs))
	}
	if author == "" {
		return fmt.Sprintf("%s%s (%s)", ticketPrefix, description, status)
	}
	return fmt.Sprintf("**%s** - %s%s (%s)", author, ticketPrefix, description, status)
}

func formatBossItem(item TemplateItem) string {
	status := normalizeStatus(item.Status)
	if status == "" {
		status = "done"
	}
	description := synthesizeDescription(item.Description)
	ticketPrefix := ""
	if strings.TrimSpace(item.TicketIDs) != "" {
		ticketPrefix = fmt.Sprintf("[%s] ", strings.TrimSpace(item.TicketIDs))
	}
	return fmt.Sprintf("%s%s (%s)", ticketPrefix, description, status)
}

func normalizeStatus(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "done":
		return "done"
	case "in testing", "in test":
		return "in testing"
	case "in progress":
		return "in progress"
	default:
		return strings.TrimSpace(status)
	}
}

func statusBucket(status string) int {
	switch normalizeStatus(status) {
	case "done":
		return 0
	case "in testing":
		return 1
	case "in progress":
		return 2
	default:
		return 3
	}
}

func stripCurrentTeamTitleFromPrefix(t *ReportTemplate, teamName string) {
	if len(t.PrefixLines) == 0 {
		return
	}
	var out []string
	for _, line := range t.PrefixLines {
		if isCurrentTeamTitleLine(line, teamName) {
			continue
		}
		out = append(out, line)
	}
	t.PrefixLines = out
}

func isCurrentTeamTitleLine(line, teamName string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "### ") {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "### "))
	parts := strings.Fields(rest)
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	if len(last) != 8 {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	name := strings.Join(parts[:len(parts)-1], " ")
	return strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(teamName))
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
