package nudge

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	ActionDone        = "nudge_done"
	ActionMore        = "nudge_more"
	ActionPagePrev    = "nudge_page_prev"
	ActionPageNext    = "nudge_page_next"
	nudgePageSize     = 10
	nudgeItemMaxRunes = 110
)

var parenPattern = regexp.MustCompile(`\([^)]*\)|（[^）]*）`)

type RenderedNudge struct {
	Text   string
	Blocks []slack.Block
}

type renderedNudgeState struct {
	page       int
	pageStart  int
	pageEnd    int
	totalPages int
}

var dayMap = map[string]time.Weekday{
	"sunday":    time.Sunday,
	"monday":    time.Monday,
	"tuesday":   time.Tuesday,
	"wednesday": time.Wednesday,
	"thursday":  time.Thursday,
	"friday":    time.Friday,
	"saturday":  time.Saturday,
}

func StartNudgeScheduler(cfg Config, db *sql.DB, api *slack.Client) {
	if len(cfg.TeamMembers) == 0 {
		log.Println("No team_members configured, nudge disabled")
		return
	}

	memberIDs, unresolved, err := resolveUserIDs(api, cfg.TeamMembers)
	if err != nil {
		log.Printf("Error resolving team_members: %v", err)
		if len(memberIDs) == 0 {
			return
		}
	}
	if len(unresolved) > 0 {
		log.Printf("Unresolved team_members: %s", strings.Join(unresolved, ", "))
	}

	weekday, ok := dayMap[strings.ToLower(cfg.NudgeDay)]
	if !ok {
		log.Printf("Invalid nudge_day '%s', using Friday", cfg.NudgeDay)
		weekday = time.Friday
	}

	hour, min, err := parseTime(cfg.NudgeTime)
	if err != nil {
		log.Printf("Invalid nudge_time '%s': %v, using 10:00", cfg.NudgeTime, err)
		hour, min = 10, 0
	}

	log.Printf("Nudge scheduled every %s at %02d:%02d for %d team members", weekday, hour, min, len(cfg.TeamMembers))

	go func() {
		for {
			now := time.Now().In(cfg.Location)
			next := nextWeekday(now, weekday, hour, min)
			wait := next.Sub(now)
			log.Printf("Next nudge at %s (in %s)", next.Format("Mon Jan 2 15:04"), wait.Round(time.Minute))

			time.Sleep(wait)
			sendNudges(api, db, cfg, memberIDs, cfg.ReportChannelID)
		}
	}()
}

func nextWeekday(now time.Time, day time.Weekday, hour, min int) time.Time {
	daysUntil := (day - now.Weekday() + 7) % 7
	if daysUntil == 0 {
		target := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if now.Before(target) {
			return target
		}
		daysUntil = 7
	}
	return time.Date(now.Year(), now.Month(), now.Day()+int(daysUntil), hour, min, 0, 0, now.Location())
}

func sendNudges(api *slack.Client, db *sql.DB, cfg Config, memberIDs []string, reportChannelID string) {
	for _, userID := range memberIDs {
		channel, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{
			Users: []string{userID},
		})
		if err != nil {
			log.Printf("Error opening DM with %s: %v", userID, err)
			continue
		}

		rendered, err := RenderNudgeForUser(api, db, cfg, userID, reportChannelID, time.Now().In(cfg.Location), 0, false)
		if err != nil {
			log.Printf("Error rendering nudge for %s: %v", userID, err)
			rendered = renderGenericNudge(cfg, reportChannelID, time.Now().In(cfg.Location))
		}

		_, _, err = api.PostMessage(
			channel.ID,
			slack.MsgOptionText(rendered.Text, false),
			slack.MsgOptionBlocks(rendered.Blocks...),
		)
		if err != nil {
			log.Printf("Error sending nudge to %s: %v", userID, err)
		} else {
			log.Printf("Sent nudge to %s", userID)
		}
	}
}

func SendNudges(api *slack.Client, db *sql.DB, cfg Config, memberIDs []string, reportChannelID string) {
	sendNudges(api, db, cfg, memberIDs, reportChannelID)
}

func RenderNudgeForUser(api *slack.Client, db *sql.DB, cfg Config, userID, reportChannelID string, now time.Time, page int, updated bool) (RenderedNudge, error) {
	now = now.In(cfg.Location)
	monday, nextMonday := ReportWeekRange(cfg, now)
	reminder := buildGenericNudgeText(reportChannelID, monday, nextMonday)

	items, err := GetItemsByDateRange(db, monday, nextMonday)
	if err != nil {
		return RenderedNudge{}, err
	}

	user, err := api.GetUserInfo(userID)
	if err != nil {
		log.Printf("nudge user lookup failed user=%s: %v", userID, err)
		user = nil
	}

	active, state := buildNudgeState(items, userID, user, page)
	if len(active) == 0 {
		if updated {
			return renderUpdatedNoItems(reminder), nil
		}
		return renderGenericNudge(cfg, reportChannelID, now), nil
	}

	return renderInteractiveNudge(reminder, userID, active, state, updated), nil
}

func renderGenericNudge(cfg Config, reportChannelID string, now time.Time) RenderedNudge {
	monday, nextMonday := ReportWeekRange(cfg, now.In(cfg.Location))
	text := buildGenericNudgeText(reportChannelID, monday, nextMonday)
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil,
			nil,
		),
	}
	return RenderedNudge{Text: text, Blocks: blocks}
}

func renderUpdatedNoItems(reminder string) RenderedNudge {
	msg := "Updated. No items are currently marked `in progress`."
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, reminder, false, false), nil, nil),
		slack.NewDividerBlock(),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, msg, false, false), nil, nil),
	}
	return RenderedNudge{
		Text:   reminder + "\n\nUpdated. No items are currently marked in progress.",
		Blocks: blocks,
	}
}

func renderInteractiveNudge(reminder, userID string, active []WorkItem, state renderedNudgeState, updated bool) RenderedNudge {
	intro := renderIntroText(len(active), updated)
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, reminder, false, false), nil, nil),
		slack.NewDividerBlock(),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, intro, false, false), nil, nil),
	}

	pageItems := active[state.pageStart:state.pageEnd]
	textLines := []string{stripMarkdownFormatting(reminder), stripMarkdownFormatting(intro)}
	for idx, item := range pageItems {
		lineNumber := state.pageStart + idx + 1
		itemText := formatNudgeItem(lineNumber, item)
		blocks = append(blocks,
			slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, itemText, false, false), nil, nil),
			slack.NewActionBlock(
				fmt.Sprintf("nudge_actions_%d", item.ID),
				buildDoneButton(userID, item.ID, state.page),
				buildMoreMenu(userID, item.ID, state.page),
			),
		)
		textLines = append(textLines, stripMarkdownFormatting(itemText))
	}

	summary := fmt.Sprintf("Showing %d-%d of %d active items", state.pageStart+1, state.pageEnd, len(active))
	blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, summary, false, false), nil, nil))
	textLines = append(textLines, summary)

	if len(active) > nudgePageSize {
		var nav []slack.BlockElement
		if state.page > 0 {
			nav = append(nav, slack.NewButtonBlockElement(
				ActionPagePrev,
				fmt.Sprintf("page|%s|%d", userID, state.page-1),
				slack.NewTextBlockObject(slack.PlainTextType, "Prev", false, false),
			))
		}
		if state.page+1 < state.totalPages {
			nav = append(nav, slack.NewButtonBlockElement(
				ActionPageNext,
				fmt.Sprintf("page|%s|%d", userID, state.page+1),
				slack.NewTextBlockObject(slack.PlainTextType, "Next", false, false),
			))
		}
		if len(nav) > 0 {
			blocks = append(blocks, slack.NewActionBlock("nudge_nav", nav...))
		}
	}

	return RenderedNudge{
		Text:   strings.Join(textLines, "\n\n"),
		Blocks: blocks,
	}
}

func buildDoneButton(userID string, itemID int64, page int) *slack.ButtonBlockElement {
	return slack.NewButtonBlockElement(
		ActionDone,
		fmt.Sprintf("done|%s|%d|%d", userID, itemID, page),
		slack.NewTextBlockObject(slack.PlainTextType, "Mark as Done", false, false),
	).WithStyle(slack.StylePrimary)
}

func buildMoreMenu(userID string, itemID int64, page int) *slack.OverflowBlockElement {
	return slack.NewOverflowBlockElement(
		ActionMore,
		slack.NewOptionBlockObject(
			fmt.Sprintf("status|%s|%d|in testing|%d", userID, itemID, page),
			slack.NewTextBlockObject(slack.PlainTextType, "Mark as In testing", false, false),
			nil,
		),
		slack.NewOptionBlockObject(
			fmt.Sprintf("status|%s|%d|in progress|%d", userID, itemID, page),
			slack.NewTextBlockObject(slack.PlainTextType, "Mark as In progress", false, false),
			nil,
		),
	)
}

func buildNudgeState(items []WorkItem, userID string, user *slack.User, requestedPage int) ([]WorkItem, renderedNudgeState) {
	active := dedupeAndFilterActiveItems(matchUserItems(items, userID, user))
	totalPages := 0
	if len(active) > 0 {
		totalPages = (len(active) + nudgePageSize - 1) / nudgePageSize
	}
	page := clampPage(requestedPage, totalPages)
	start, end := pageBounds(len(active), page)
	return active, renderedNudgeState{
		page:       page,
		pageStart:  start,
		pageEnd:    end,
		totalPages: totalPages,
	}
}

func matchUserItems(items []WorkItem, userID string, user *slack.User) []WorkItem {
	var matched []WorkItem
	for _, item := range items {
		if itemBelongsToUser(item, userID, user) {
			matched = append(matched, item)
		}
	}
	return matched
}

func itemBelongsToUser(item WorkItem, userID string, user *slack.User) bool {
	if strings.TrimSpace(item.AuthorID) != "" {
		return strings.TrimSpace(item.AuthorID) == strings.TrimSpace(userID)
	}
	if user == nil {
		return false
	}
	for _, candidate := range userNameCandidates(user) {
		if strings.EqualFold(strings.TrimSpace(item.Author), candidate) || nameMatches(item.Author, candidate) || nameMatches(candidate, item.Author) {
			return true
		}
	}
	return false
}

func userNameCandidates(user *slack.User) []string {
	if user == nil {
		return nil
	}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range out {
			if strings.EqualFold(existing, s) {
				return
			}
		}
		out = append(out, s)
	}
	add(user.Profile.DisplayName)
	add(user.RealName)
	add(user.Name)
	return out
}

func dedupeAndFilterActiveItems(items []WorkItem) []WorkItem {
	latest := make(map[string]WorkItem, len(items))
	for _, item := range items {
		key := nudgeDedupeKey(item)
		if key == "|" {
			continue
		}
		existing, ok := latest[key]
		if !ok || item.ReportedAt.After(existing.ReportedAt) || (item.ReportedAt.Equal(existing.ReportedAt) && item.ID > existing.ID) {
			latest[key] = item
		}
	}

	active := make([]WorkItem, 0, len(latest))
	for _, item := range latest {
		if statusContainsInProgress(item.Status) {
			active = append(active, item)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if !active[i].ReportedAt.Equal(active[j].ReportedAt) {
			return active[i].ReportedAt.Before(active[j].ReportedAt)
		}
		return active[i].ID < active[j].ID
	})
	return active
}

func nudgeDedupeKey(item WorkItem) string {
	tickets := canonicalTicketIDs(item.TicketIDs)
	desc := normalizeDescriptionForDedupe(item.Description, tickets)
	return tickets + "|" + desc
}

func normalizeDescriptionForDedupe(description, tickets string) string {
	description = strings.TrimSpace(stripLeadingTicketPrefixIfSame(description, tickets))
	return strings.ToLower(description)
}

func statusContainsInProgress(status string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(status)), "in progress")
}

func pageBounds(total, page int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	start := page * nudgePageSize
	if start > total {
		start = total
	}
	end := start + nudgePageSize
	if end > total {
		end = total
	}
	return start, end
}

func clampPage(page, totalPages int) int {
	if totalPages <= 0 {
		return 0
	}
	if page < 0 {
		return 0
	}
	if page >= totalPages {
		return totalPages - 1
	}
	return page
}

func renderIntroText(count int, updated bool) string {
	if updated {
		if count == 1 {
			return "Updated. This item is still marked `in progress`:"
		}
		return "Updated. These items are still marked `in progress`:"
	}
	if count == 1 {
		return "This item is still marked `in progress`. Please update the status if it has changed."
	}
	return "These items are still marked `in progress`. Please update the status if any have changed."
}

func buildGenericNudgeText(reportChannelID string, monday, nextMonday time.Time) string {
	channelRef := ""
	if reportChannelID != "" {
		channelRef = fmt.Sprintf(" Please report in <#%s>.", reportChannelID)
	}
	return fmt.Sprintf(
		"Hey! Friendly reminder to report your work items for this week (%s - %s) using `/report`.%s\n"+
			"Example: `/report [ticket_id] Add pagination to user list API (done)`",
		monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"),
		channelRef,
	)
}

func formatNudgeItem(lineNumber int, item WorkItem) string {
	description := strings.TrimSpace(item.Description)
	tickets := canonicalTicketIDs(item.TicketIDs)
	if tickets != "" {
		description = stripLeadingTicketPrefixIfSame(description, tickets)
		description = fmt.Sprintf("[%s] %s", tickets, description)
	}
	statusText := strings.TrimSpace(item.Status)
	suffix := fmt.Sprintf(" _(current: %s)_", statusText)
	prefix := fmt.Sprintf("%d. ", lineNumber)
	description = truncateRunes(description, nudgeItemMaxRunes-runeLen(prefix)-runeLen(suffix))
	return fmt.Sprintf("%s%s%s", prefix, description, suffix)
}

func canonicalTicketIDs(ticketIDs string) string {
	if strings.TrimSpace(ticketIDs) == "" {
		return ""
	}
	parts := strings.Split(ticketIDs, ",")
	cleaned := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, "[]"))
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, part)
	}
	return strings.Join(cleaned, ",")
}

func stripLeadingTicketPrefixIfSame(description, tickets string) string {
	description = strings.TrimSpace(description)
	if description == "" || tickets == "" {
		return description
	}

	if strings.HasPrefix(description, "[") {
		end := strings.Index(description, "]")
		if end > 1 {
			leading := canonicalTicketIDs(description[1:end])
			if strings.EqualFold(leading, tickets) {
				return strings.TrimSpace(description[end+1:])
			}
		}
	}
	return description
}

func stripMarkdownFormatting(s string) string {
	replacer := strings.NewReplacer("`", "", "*", "", "_", "")
	return replacer.Replace(s)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

func runeLen(s string) int {
	return len([]rune(s))
}

func normalizeNameTokens(s string) []string {
	if s == "" {
		return nil
	}
	s = parenPattern.ReplaceAllString(s, " ")
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	parts := strings.Fields(b.String())
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func nameMatches(a, b string) bool {
	at := normalizeNameTokens(a)
	bt := normalizeNameTokens(b)
	if len(at) == 0 || len(bt) == 0 {
		return false
	}
	return allIn(at, bt) || allIn(bt, at)
}

func allIn(needles, haystack []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, t := range haystack {
		set[t] = true
	}
	for _, t := range needles {
		if !set[t] {
			return false
		}
	}
	return true
}

func parseTime(s string) (int, int, error) {
	var hour, min int
	_, err := fmt.Sscanf(s, "%d:%d", &hour, &min)
	if err != nil {
		return 0, 0, err
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return 0, 0, fmt.Errorf("time out of range: %02d:%02d", hour, min)
	}
	return hour, min, nil
}
