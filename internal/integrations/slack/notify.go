package slackbot

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"

	"reportbot/internal/domain"
	"reportbot/internal/fetch"
	"reportbot/internal/filing"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

// Notifier tells reporters and authors what filing decided.
type Notifier struct {
	API *slack.Client
	DB  *sql.DB
	Cfg Config
}

func NewNotifier(api *slack.Client, db *sql.DB, cfg Config) *Notifier {
	return &Notifier{API: api, DB: db, Cfg: cfg}
}

// FetchHooks binds Code Change authors through author_ids only, and wakes filing.
func FetchHooks(cfg Config) fetch.Hooks {
	return fetch.Hooks{ResolveAuthorID: cfg.AuthorID, Wake: func() { wakeFiling() }}
}

func liveReportLink(cfg Config) string {
	if !cfg.WebEnabled || cfg.WebBaseURL == "" {
		return ""
	}
	return fmt.Sprintf("<%s/|Open the live report>", strings.TrimRight(cfg.WebBaseURL, "/"))
}

func mdSection(text string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
}

func button(actionID, value, label string) *slack.ButtonBlockElement {
	if len([]rune(label)) > 75 {
		label = string([]rune(label)[:72]) + "..."
	}
	return slack.NewButtonBlockElement(actionID, value, slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
}

// FiledBlocks is the reply about one item: where it went, or where it might go.
func FiledBlocks(cfg Config, r filing.Result) (string, []slack.Block) {
	line := slackLine(r.Item)
	var text string
	var blocks []slack.Block
	switch {
	case r.Failed:
		text = "Saved, but filing failed (model unreachable). It is in Undetermined and will be retried automatically.\n" + line
		blocks = append(blocks, mdSection(text))
	case r.Section != nil:
		text = fmt.Sprintf("Filed under *%s*\n%s", r.Section.Path(), line)
		details := []string{"Status: " + r.Item.Status}
		if r.Item.StatusNote != "" {
			details[0] += fmt.Sprintf(" · note: \"%s\"", r.Item.StatusNote)
		}
		if r.Item.TicketIDs != "" {
			details = append(details, "Tickets: "+r.Item.TicketIDs)
		}
		text += "\n" + strings.Join(details, "\n")
		blocks = append(blocks, mdSection(text))
	default:
		text = "*Where does this go?*\n" + line
		blocks = append(blocks, mdSection(text))
		var btns []slack.BlockElement
		for _, s := range r.Choices {
			btns = append(btns, button(actionPlacePick, fmt.Sprintf("%d:%d", r.Item.ID, s.ID), s.Label()))
		}
		btns = append(btns, button(actionPlaceOther, strconv.FormatInt(r.Item.ID, 10), "Other…"))
		blocks = append(blocks, slack.NewActionBlock(fmt.Sprintf("place_%d", r.Item.ID), btns...),
			slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "It stays in Undetermined until someone picks.", false, false)))
	}
	if r.Update != nil {
		u := "Looks like an update to: " + slackLine(r.Update.Item)
		text += "\n" + u
		value := fmt.Sprintf("%d:%d", r.Update.Item.ID, r.Item.ID)
		blocks = append(blocks, mdSection(u), slack.NewActionBlock(fmt.Sprintf("update_%d", r.Item.ID),
			button(actionUpdateConfirm, value, "Update it"), button(actionUpdateReject, value, "It's new")))
	}
	if link := liveReportLink(cfg); link != "" {
		blocks = append(blocks, slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, link, false, false)))
	}
	return text, blocks
}

// Filed implements filing.Notifier.
func (n *Notifier) Filed(r filing.Result) {
	if r.Payload.Kind == "code_change" {
		n.codeChangeFiled(r)
		return
	}
	text, blocks := FiledBlocks(n.Cfg, r)
	p := r.Payload
	if p.ResponseURL != "" || p.ChannelID != "" {
		respond(n.API, p.ResponseURL, p.ChannelID, p.UserID, p.Replace, text, blocks)
		return
	}
	// Sweep retries have nowhere to reply: tell the author directly when there is something to decide.
	if r.Section == nil || r.Update != nil {
		dm(n.API, r.Item.AuthorID, text, blocks)
	}
}

func (n *Notifier) codeChangeFiled(r filing.Result) {
	if r.Item.AuthorID == "" {
		return
	}
	cc, err := ownCodeChange(n.DB, r.Item.ID)
	if err != nil {
		return
	}
	if r.Link != nil {
		text := fmt.Sprintf("Your merge request %q looks like part of:\n%s\nIs this part of it?", cc.Title, slackLine(r.Link.Item))
		dm(n.API, r.Item.AuthorID, text, []slack.Block{mdSection(text), slack.NewActionBlock(fmt.Sprintf("link_%d", cc.ID),
			button(actionLinkConfirm, fmt.Sprintf("%d:%d", cc.ID, r.Link.Item.ID), "Link"),
			button(actionLinkReject, strconv.FormatInt(cc.ID, 10), "Separate"))})
		return
	}
	if r.Section == nil {
		text, blocks := FiledBlocks(n.Cfg, r)
		head := fmt.Sprintf("Your merge request <%s|%s>:", cc.SourceRef, cc.Title)
		dm(n.API, r.Item.AuthorID, head+"\n"+text, append([]slack.Block{mdSection(head)}, blocks...))
	}
}

func ownCodeChange(db *sql.DB, itemID int64) (domain.CodeChange, error) {
	ccs, err := sqlite.GetCodeChangesForItem(db, itemID)
	if err != nil {
		return domain.CodeChange{}, err
	}
	for _, c := range ccs {
		if c.LinkKind != domain.LinkLinked {
			return c, nil
		}
	}
	return domain.CodeChange{}, sql.ErrNoRows
}

// PlacementOverridden implements filing.Notifier: a re-classification moved a person's choice.
func (n *Notifier) PlacementOverridden(item domain.WorkItem, eventID int64, from, to domain.Section) {
	text := fmt.Sprintf("After a report structure change, your item moved:\n%s\n%s → %s",
		slackLine(item), from.Label(), to.Label())
	dm(n.API, item.AuthorID, text, []slack.Block{mdSection(text),
		slack.NewActionBlock(fmt.Sprintf("undo_%d", eventID), button(actionPlacementUndo, strconv.FormatInt(eventID, 10), "Undo"))})
}

// respond replies through a response URL, falling back to an ephemeral message.
func respond(api *slack.Client, responseURL, channelID, userID string, replace bool, text string, blocks []slack.Block) {
	if responseURL != "" {
		msg := &slack.WebhookMessage{Text: text, ResponseType: slack.ResponseTypeEphemeral, ReplaceOriginal: replace}
		if len(blocks) > 0 {
			msg.Blocks = &slack.Blocks{BlockSet: blocks}
		}
		if err := slack.PostWebhook(responseURL, msg); err == nil {
			return
		} else {
			log.Printf("slack respond via response_url failed: %v", err)
		}
	}
	if channelID == "" || userID == "" {
		dm(api, userID, text, blocks)
		return
	}
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	if _, err := api.PostEphemeral(channelID, userID, opts...); err != nil {
		log.Printf("slack respond ephemeral failed, sending DM: %v", err)
		dm(api, userID, text, blocks)
	}
}

func dm(api *slack.Client, userID, text string, blocks []slack.Block) {
	if strings.TrimSpace(userID) == "" {
		return
	}
	ch, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{userID}})
	if err != nil {
		log.Printf("slack dm open error user=%s: %v", userID, err)
		return
	}
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if len(blocks) > 0 {
		opts = append(opts, slack.MsgOptionBlocks(blocks...))
	}
	if _, _, err := api.PostMessage(ch.ID, opts...); err != nil {
		log.Printf("slack dm post error user=%s: %v", userID, err)
	}
}

// replyToAction replaces the message a button was on, or answers ephemerally.
func replyToAction(api *slack.Client, cb slack.InteractionCallback, text string) {
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	respond(api, cb.ResponseURL, channelID, cb.User.ID, true, text, nil)
}

func parsePair(value string) (int64, int64, bool) {
	a, b, ok := strings.Cut(strings.TrimSpace(value), ":")
	if !ok {
		return 0, 0, false
	}
	x, err1 := strconv.ParseInt(a, 10, 64)
	y, err2 := strconv.ParseInt(b, 10, 64)
	return x, y, err1 == nil && err2 == nil
}

// canActOn: managers act on every item, others on items whose Slack user ID is theirs.
func canActOn(cfg Config, userID string, item WorkItem) bool {
	return canManageItem(item, cfg.IsManagerID(userID), userID)
}

// slackLine is the report line in Slack's mrkdwn, where bold is a single asterisk.
func slackLine(item WorkItem) string {
	return strings.Replace(report.FormatTeamItem(item), "**", "*", 2)
}

// handlePlacePick files an item where the person chose ("Where does this go?").
func handlePlacePick(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	itemID, sectionID, ok := parsePair(value)
	if !ok {
		return
	}
	item, err := GetWorkItemByID(db, itemID)
	if err != nil {
		replyToAction(api, cb, "That item no longer exists.")
		return
	}
	if !canActOn(cfg, cb.User.ID, item) {
		replyToAction(api, cb, "You can only file your own items.")
		return
	}
	tax, err := LoadTaxonomy(db)
	if err != nil {
		return
	}
	sec, ok := tax.Section(sectionID)
	if !ok {
		replyToAction(api, cb, "That Section no longer exists. Use Other… to pick another.")
		return
	}
	if err := sqlite.SetPlacement(db, itemID, sqlite.Placement{SectionID: sectionID, PlacedBy: domain.PlacedByPerson,
		Confidence: item.Confidence, HistoryNote: sqlite.ReasonButton, ActorID: cb.User.ID}); err != nil {
		log.Printf("place pick error item=%d: %v", itemID, err)
		return
	}
	replyToAction(api, cb, fmt.Sprintf("Filed under *%s*\n%s", sec.Path(), slackLine(item)))
}

// handleUpdateConfirm applies an Update: the existing item takes the new report's text and Status.
func handleUpdateConfirm(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	existingID, newID, ok := parsePair(value)
	if !ok {
		return
	}
	newItem, err := GetWorkItemByID(db, newID)
	if err != nil {
		replyToAction(api, cb, "This report was already handled.")
		return
	}
	existing, err := GetWorkItemByID(db, existingID)
	if err != nil {
		replyToAction(api, cb, "The earlier item no longer exists; the report stays a new item.")
		return
	}
	if !canActOn(cfg, cb.User.ID, newItem) || !canActOn(cfg, cb.User.ID, existing) {
		replyToAction(api, cb, "You can only update your own items.")
		return
	}
	merged, err := sqlite.MergeUpdate(db, existingID, newID)
	if err != nil {
		log.Printf("update confirm error existing=%d new=%d: %v", existingID, newID, err)
		replyToAction(api, cb, fmt.Sprintf("Update failed: %v", err))
		return
	}
	replyToAction(api, cb, "Updated:\n"+slackLine(merged))
	log.Printf("update confirmed user=%s existing=%d new=%d", cb.User.ID, existingID, newID)
	notifyManagersOfUpdate(api, cfg, cb.User.ID, merged)
}

func notifyManagersOfUpdate(api *slack.Client, cfg Config, userID string, item WorkItem) {
	if cfg.IsManagerID(userID) {
		return
	}
	msg := fmt.Sprintf("Update from *%s*:\n• %s", item.Author, slackLine(item))
	for _, managerID := range cfg.ManagerSlackIDs {
		if managerID = strings.TrimSpace(managerID); managerID != "" && managerID != userID {
			dm(api, managerID, msg, nil)
		}
	}
}

func handleUpdateReject(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	_, newID, ok := parsePair(value)
	if !ok {
		return
	}
	if item, err := GetWorkItemByID(db, newID); err == nil {
		replyToAction(api, cb, "Kept as a new item:\n"+slackLine(item))
	}
}

func handleLinkConfirm(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	ccID, target, ok := parsePair(value)
	if !ok {
		return
	}
	cc, err := sqlite.GetCodeChange(db, ccID)
	if err != nil || (cc.AuthorID != cb.User.ID && !cfg.IsManagerID(cb.User.ID)) {
		return
	}
	if err := sqlite.ConfirmCodeChangeLink(db, ccID, target); err != nil {
		log.Printf("link confirm error cc=%d target=%d: %v", ccID, target, err)
		replyToAction(api, cb, fmt.Sprintf("Link failed: %v", err))
		return
	}
	if cc.State == "merged" {
		if it, err := GetWorkItemByID(db, target); err == nil && it.Status == domain.StatusInProgress {
			_ = SetWorkItemStatus(db, target, domain.StatusInTesting)
		}
	}
	replyToAction(api, cb, fmt.Sprintf("Linked %q to the existing item.", cc.Title))
}

func handleLinkReject(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	ccID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	cc, err := sqlite.GetCodeChange(db, ccID)
	if err != nil || (cc.AuthorID != cb.User.ID && !cfg.IsManagerID(cb.User.ID)) {
		return
	}
	if err := sqlite.RejectCodeChangeLink(db, ccID); err != nil {
		log.Printf("link reject error cc=%d: %v", ccID, err)
		return
	}
	replyToAction(api, cb, fmt.Sprintf("Kept %q as its own item.", cc.Title))
}

// handlePlacementUndo restores the Placement a re-classification replaced.
func handlePlacementUndo(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	eventID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	ev, err := sqlite.GetPlacementEvent(db, eventID)
	if err != nil {
		return
	}
	item, err := GetWorkItemByID(db, ev.WorkItemID)
	if err != nil {
		replyToAction(api, cb, "That item no longer exists.")
		return
	}
	if !canActOn(cfg, cb.User.ID, item) {
		return
	}
	if item.SectionID != ev.ToSection {
		replyToAction(api, cb, "This item has moved again since; nothing to undo.")
		return
	}
	if err := sqlite.SetPlacement(db, item.ID, sqlite.Placement{SectionID: ev.FromSection, PlacedBy: domain.PlacedByPerson,
		HistoryNote: sqlite.ReasonUndo, ActorID: cb.User.ID}); err != nil {
		log.Printf("undo error event=%d: %v", eventID, err)
		return
	}
	replyToAction(api, cb, "Undone:\n"+slackLine(item))
}

// mergedInTestingBlocks lists merged Code Changes waiting in testing, with Done buttons.
func mergedInTestingBlocks(db *sql.DB) []slack.Block {
	open, err := GetOpenItems(db)
	if err != nil {
		log.Printf("check: open items error: %v", err)
		return nil
	}
	var waiting []WorkItem
	for _, it := range open {
		if (it.Source == "gitlab" || it.Source == "github") && it.Status == domain.StatusInTesting {
			waiting = append(waiting, it)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	blocks := []slack.Block{slack.NewDividerBlock(), mdSection(fmt.Sprintf("*Merged code changes waiting in testing (%d)*", len(waiting)))}
	for i, it := range waiting {
		if i == 40 { // Slack allows 50 blocks per message
			blocks = append(blocks, mdSection(fmt.Sprintf("… and %d more", len(waiting)-i)))
			break
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, slackLine(it), false, false), nil,
			slack.NewAccessory(button(actionCheckDone, strconv.FormatInt(it.ID, 10), "Done"))))
	}
	return blocks
}

func handleCheckDone(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string) {
	if !cfg.IsManagerID(cb.User.ID) {
		return
	}
	itemID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	if err := SetWorkItemStatus(db, itemID, domain.StatusDone); err != nil {
		log.Printf("check done error item=%d: %v", itemID, err)
		return
	}
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	if item, err := GetWorkItemByID(db, itemID); err == nil {
		postEphemeralTo(api, channelID, cb.User.ID, "Marked done: "+slackLine(item))
	}
}

// editModalBlocks builds the edit form: description, Status, Status Note and Section.
func editModalBlocks(item WorkItem, tax domain.Taxonomy) []slack.Block {
	descInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "Description", false, false), editActionDescription,
	).WithInitialValue(item.Description)

	var statusOptions []*slack.OptionBlockObject
	var initialStatus *slack.OptionBlockObject
	for _, st := range domain.Statuses {
		opt := slack.NewOptionBlockObject(st, slack.NewTextBlockObject(slack.PlainTextType, st, false, false), nil)
		statusOptions = append(statusOptions, opt)
		if st == item.Status {
			initialStatus = opt
		}
	}
	statusSelect := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic,
		slack.NewTextBlockObject(slack.PlainTextType, "Status", false, false), editActionStatus, statusOptions...)
	if initialStatus != nil {
		statusSelect.InitialOption = initialStatus
	}

	noteInput := slack.NewPlainTextInputBlockElement(
		slack.NewTextBlockObject(slack.PlainTextType, "e.g. root cause analysis in progress", false, false), editActionStatusNote,
	).WithInitialValue(item.StatusNote)
	noteBlock := slack.NewInputBlock(editBlockStatusNote, slack.NewTextBlockObject(slack.PlainTextType, "Status note", false, false), nil, noteInput)
	noteBlock.Optional = true

	// Sections grouped by "Area › Category"; Slack allows 100 options per group.
	var groups []*slack.OptionGroupBlockObject
	var initialSection *slack.OptionBlockObject
	groupIndex := map[string]int{}
	for _, s := range tax.Sections() {
		label := truncateLabel(s.Area + domain.PathSeparator + s.Category)
		gi, ok := groupIndex[label]
		if !ok {
			groups = append(groups, slack.NewOptionGroupBlockElement(slack.NewTextBlockObject(slack.PlainTextType, label, false, false)))
			gi = len(groups) - 1
			groupIndex[label] = gi
		}
		if len(groups[gi].Options) >= 100 {
			continue
		}
		opt := slack.NewOptionBlockObject(strconv.FormatInt(s.ID, 10), slack.NewTextBlockObject(slack.PlainTextType, truncateLabel(s.Label()), false, false), nil)
		groups[gi].Options = append(groups[gi].Options, opt)
		if s.ID == item.SectionID {
			initialSection = opt
		}
	}
	und := slack.NewOptionBlockObject(undeterminedOptValue, slack.NewTextBlockObject(slack.PlainTextType, report.UndeterminedHeading, false, false), nil)
	groups = append(groups, slack.NewOptionGroupBlockElement(slack.NewTextBlockObject(slack.PlainTextType, "Not sure", false, false), und))
	if initialSection == nil {
		initialSection = und
	}
	sectionSelect := slack.NewOptionsGroupSelectBlockElement(slack.OptTypeStatic,
		slack.NewTextBlockObject(slack.PlainTextType, "Section", false, false), editActionSection, groups...)
	sectionSelect.InitialOption = initialSection

	return []slack.Block{
		slack.NewInputBlock(editBlockDescription, slack.NewTextBlockObject(slack.PlainTextType, "Description", false, false), nil, descInput),
		slack.NewInputBlock(editBlockStatus, slack.NewTextBlockObject(slack.PlainTextType, "Status", false, false), nil, statusSelect),
		noteBlock,
		slack.NewInputBlock(editBlockSection, slack.NewTextBlockObject(slack.PlainTextType, "Section", false, false), nil, sectionSelect),
	}
}

func truncateLabel(s string) string {
	if r := []rune(s); len(r) > 75 {
		return string(r[:72]) + "..."
	}
	return s
}
