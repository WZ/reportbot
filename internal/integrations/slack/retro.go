package slackbot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/learning"
	"reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

// Loops is the learning loops as Slack drives them (*learning.Learner in production).
type Loops interface {
	RunNow(ctx context.Context) (learning.RunNowReport, error)
	UndoNotes(historyID int64, actorID string) (domain.Section, error)
	ApproveProposal(id int64, actorID string) (learning.StoredProposal, error)
	RejectProposal(id int64, actorID string) error
	SweepUndetermined(ctx context.Context, items []domain.WorkItem, timeout time.Duration) int
	ProposeStructure(ctx context.Context) ([]learning.StoredProposal, error)
}

// loops is nil when no LLM is configured; set by StartSlackBot.
var loops Loops

const (
	actionNotesUndo       = "notes_undo"
	actionProposalApprove = "proposal_approve"
	actionProposalReject  = "proposal_reject"
	retrospectTimeout     = 30 * time.Minute
	noLLMText             = "The LLM isn't configured (llm_provider and its API key), so the learning loops and /retrospect are off. Filing works as usual."
)

// handleRetrospective runs the learning loops now: Learned Notes behind the replay gate, then
// Structure Proposals, and shows what was applied, discarded or proposed.
func handleRetrospective(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("retrospective denied user=%s", cmd.UserID)
		return
	}
	if loops == nil || !cfg.LLMConfigured() {
		postEphemeral(api, cmd, noLLMText)
		return
	}
	postEphemeral(api, cmd, "Running the learning loops (this can take a few minutes)…")
	ctx, cancel := context.WithTimeout(context.Background(), retrospectTimeout)
	defer cancel()
	rep, err := loops.RunNow(ctx)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("The learning loops failed: %v", err))
		return
	}
	tax, _ := LoadTaxonomy(db)
	text, blocks := retrospectBlocks(rep, tax)
	if _, err := api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...)); err != nil {
		log.Printf("retrospective reply error: %v", err)
	}
}

func gainText(g *learning.GateResult) string {
	if g == nil {
		return ""
	}
	return fmt.Sprintf("%+.1f pts on %d recent Work Items", g.GainPoints(), g.Items)
}

func retrospectBlocks(rep learning.RunNowReport, tax domain.Taxonomy) (string, []slack.Block) {
	n := rep.Notes
	var notes string
	switch {
	case rep.NotesErr != nil && n.Decision == "":
		notes = fmt.Sprintf("*Learned notes:* failed: %v", rep.NotesErr)
	case n.Applied:
		notes = "*Learned notes applied* (" + gainText(n.Gate) + ")"
	case n.Decision == sqlite.RunDiscarded:
		notes = "*Learned notes discarded:* " + n.Reason
		for _, c := range n.Candidates {
			notes += fmt.Sprintf("\n• %s: _%s_", c.Section.Label(), c.New)
		}
		for _, c := range n.Invalid {
			notes += fmt.Sprintf("\n• %s: _%s_ (invalid: %s)", c.Section.Label(), c.Notes, c.Reason)
		}
	default:
		notes = fmt.Sprintf("*Learned notes:* nothing to learn (%s).", n.Reason)
	}
	blocks := []slack.Block{mdSection(notes)}
	if n.Applied {
		blocks = append(blocks, undoBlocks(n)...)
	}
	var props string
	switch {
	case rep.PropErr != nil:
		props = fmt.Sprintf("*Structure Proposals:* failed: %v", rep.PropErr)
	case len(rep.Proposals) == 0:
		props = "*Structure Proposals:* none this time."
	default:
		props = fmt.Sprintf("*Structure Proposals sent to managers (%d):*", len(rep.Proposals))
		for _, p := range rep.Proposals {
			props += "\n• " + proposalText(tax, p)
		}
	}
	return notes + "\n" + props, append(blocks, mdSection(props))
}

// undoBlocks lists each applied Section with its notes and an Undo button.
func undoBlocks(r learning.NotesReport) []slack.Block {
	var out []slack.Block
	for _, c := range r.Candidates {
		if c.HistoryID == 0 {
			continue
		}
		out = append(out, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("*%s*: _%s_", c.Section.Label(), c.New), false, false), nil,
			slack.NewAccessory(button(actionNotesUndo, strconv.FormatInt(c.HistoryID, 10), "Undo"))))
	}
	return out
}

// NotesDigest is the nightly DM to managers after Learned Notes were applied.
func NotesDigest(r learning.NotesReport) (string, []slack.Block) {
	labels := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		labels = append(labels, c.Section.Label())
	}
	text := fmt.Sprintf("Learned notes updated: %s (%s)", strings.Join(labels, ", "), gainText(r.Gate))
	blocks := append([]slack.Block{mdSection(text)}, undoBlocks(r)...)
	return text, append(blocks, contextLine("The decision model reads these notes from now on. Any manager can Undo."))
}

// proposalText describes one Structure Proposal and how many Work Items it affects.
func proposalText(tax domain.Taxonomy, p learning.StoredProposal) string {
	var s string
	switch p.Kind {
	case "new":
		parent := fmt.Sprintf("node %d", p.ParentID)
		if n, ok := tax.Node(p.ParentID); ok {
			parent = n.Name
			if a, ok := tax.Node(n.ParentID); ok {
				parent = a.Name + domain.PathSeparator + n.Name
			}
		}
		s = fmt.Sprintf("*New Section* “%s” under %s", p.Name, parent)
		if p.Description != "" {
			s += " — " + p.Description
		}
		s += fmt.Sprintf(". %d Work Items look like it.", p.Affected)
	case "split":
		names := make([]string, len(p.NewSections))
		for i, ns := range p.NewSections {
			names[i] = "“" + ns.Name + "”"
		}
		s = fmt.Sprintf("*Split* %s into %s. %d Open Work Items would be re-filed.", sectionName(tax, p.SectionID), strings.Join(names, ", "), p.Affected)
	case "retire":
		s = fmt.Sprintf("*Retire* %s: no new Work Items for 8 weeks and none open.", sectionName(tax, p.SectionID))
	}
	if p.Reason != "" {
		s += "\n_" + p.Reason + "_"
	}
	return s
}

func sectionName(tax domain.Taxonomy, id int64) string {
	if s, ok := tax.SectionAny(id); ok {
		return s.Path()
	}
	return fmt.Sprintf("Section %d", id)
}

// ProposalBlocks is the DM to managers with [Approve] [Reject] per proposal.
func ProposalBlocks(tax domain.Taxonomy, ps []learning.StoredProposal) (string, []slack.Block) {
	text := fmt.Sprintf("%d report structure proposal(s) from this week's Work Items", len(ps))
	blocks := []slack.Block{mdSection("*" + text + "*")}
	for _, p := range ps {
		id := strconv.FormatInt(p.ID, 10)
		blocks = append(blocks, mdSection(proposalText(tax, p)), slack.NewActionBlock("proposal_"+id,
			button(actionProposalApprove, id, "Approve").WithStyle(slack.StylePrimary),
			button(actionProposalReject, id, "Reject")))
	}
	return text, append(blocks, contextLine("Approving applies it like a Taxonomy edit and re-files Open Work Items. A rejected change is not proposed again for 8 weeks."))
}

func handleNotesUndo(api *slack.Client, cfg Config, cb slack.InteractionCallback, value string) {
	if !cfg.IsManagerID(cb.User.ID) || loops == nil {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	sec, err := loops.UndoNotes(id, cb.User.ID)
	switch {
	case errors.Is(err, sqlite.ErrNothingToUndo):
		replyToAction(api, cb, "Nothing to undo: these learned notes were already undone or have changed since.")
	case err != nil:
		replyToAction(api, cb, fmt.Sprintf("Undo failed: %v", err))
	default:
		replyToAction(api, cb, fmt.Sprintf("Undone: the learned notes of *%s* are back to what they were.", sec.Label()))
	}
}

func handleProposalDecision(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string, approve bool) {
	if !cfg.IsManagerID(cb.User.ID) || loops == nil {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	if !approve {
		switch err := loops.RejectProposal(id, cb.User.ID); {
		case errors.Is(err, learning.ErrAlreadyDecided):
			replyToAction(api, cb, "This proposal was already decided by another manager.")
		case err != nil:
			replyToAction(api, cb, fmt.Sprintf("Reject failed: %v", err))
		default:
			replyToAction(api, cb, "Rejected. The same change won't be proposed again for 8 weeks.")
		}
		return
	}
	p, err := loops.ApproveProposal(id, cb.User.ID)
	switch {
	case errors.Is(err, learning.ErrAlreadyDecided):
		replyToAction(api, cb, "This proposal was already decided by another manager.")
	case err != nil:
		replyToAction(api, cb, fmt.Sprintf("Approve failed: %v", err))
	default:
		tax, _ := LoadTaxonomy(db)
		replyToAction(api, cb, "Approved: "+proposalText(tax, p)+"\nOpen Work Items will be re-filed.")
	}
}

// LearningNotifier implements learning.Notifier over Slack DMs.
type LearningNotifier struct {
	API *slack.Client
	DB  *sql.DB
	Cfg Config
}

func (n *LearningNotifier) managers() []string {
	var out []string
	for _, id := range n.Cfg.ManagerSlackIDs {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func (n *LearningNotifier) NotesApplied(r learning.NotesReport) {
	text, blocks := NotesDigest(r)
	for _, id := range n.managers() {
		dm(n.API, id, text, blocks)
	}
}

func (n *LearningNotifier) ProposalsReady(ps []learning.StoredProposal) {
	tax, _ := LoadTaxonomy(n.DB)
	text, blocks := ProposalBlocks(tax, ps)
	for _, id := range n.managers() {
		dm(n.API, id, text, blocks)
	}
}

// AssistantFiled tells the reporter where the assistant filed their unsure Work Item.
func (n *LearningNotifier) AssistantFiled(item domain.WorkItem, s domain.Section) {
	head := fmt.Sprintf("Filed under *%s* by the assistant", s.Label())
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, head+"\n"+itemQuote(item, false), false, false), nil,
			slack.NewAccessory(button(actionPlaceOther, strconv.FormatInt(item.ID, 10), "Change"))),
	}
	if parent := sectionParent(s); parent != "" {
		blocks = append(blocks, contextLine(parent))
	}
	dm(n.API, item.AuthorID, head+": "+slackLine(item), blocks)
}

// handleReportStats shows this week's filing: arrivals, auto-filed vs asked, corrections, Undetermined.
func handleReportStats(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	st, err := getWeekStats(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading stats: %v", err))
		return
	}
	tax, _ := LoadTaxonomy(db)
	postEphemeral(api, cmd, formatStats(st, tax, monday, nextMonday, cfg.LLMConfigured()))
}

func formatStats(st WeekStats, tax domain.Taxonomy, monday, nextMonday time.Time, llmOn bool) string {
	pct := func(n int) string {
		if st.ItemsThisWeek == 0 {
			return "0%"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(st.ItemsThisWeek))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*Filing for %s - %s*\n", monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"))
	fmt.Fprintf(&b, "- Work Items reported or fetched: %d\n", st.ItemsThisWeek)
	fmt.Fprintf(&b, "- Filed automatically: %d (%s)\n", st.FiledByModel, pct(st.FiledByModel))
	fmt.Fprintf(&b, "- Asked \"Where does this go?\": %d (%s)\n", st.Asked, pct(st.Asked))
	fmt.Fprintf(&b, "- Filed by the assistant: %d\n", st.FiledByAssistant)
	if st.FailedFiling > 0 {
		fmt.Fprintf(&b, "- Filing failed (model unreachable): %d\n", st.FailedFiling)
	}
	fmt.Fprintf(&b, "- Undetermined on the report now: %d\n", st.Undetermined)
	if !llmOn {
		b.WriteString("_Learning loops are off: no LLM configured._\n")
	}
	if len(st.PersonCorrections) > 0 {
		type kv struct {
			id int64
			n  int
		}
		var rows []kv
		for id, n := range st.PersonCorrections {
			rows = append(rows, kv{id, n})
		}
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].n > rows[j].n || (rows[i].n == rows[j].n && rows[i].id < rows[j].id)
		})
		b.WriteString("\n*Corrections by the model's Section*\n")
		for _, r := range rows {
			name := fmt.Sprintf("Section %d", r.id)
			if s, ok := tax.SectionAny(r.id); ok {
				name = s.Path()
			}
			fmt.Fprintf(&b, "- %s: %d\n", name, r.n)
		}
	}
	return b.String()
}
