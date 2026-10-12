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

	"github.com/slack-go/slack"
)

// Proposals is Structure Proposals as Slack drives them (*learning.Learner in production).
type Proposals interface {
	ProposeStructure(ctx context.Context) ([]learning.StoredProposal, error)
	ApproveProposal(id int64, actorID string) (learning.StoredProposal, error)
	RejectProposal(id int64, actorID string) error
}

// proposals is nil when no LLM is configured; set by StartSlackBot.
var proposals Proposals

const (
	actionProposalApprove = "proposal_approve"
	actionProposalReject  = "proposal_reject"
	retrospectTimeout     = 30 * time.Minute
	noLLMText             = "The LLM isn't configured (llm_provider and its API key), so Structure Proposals and /retrospect are off. Filing works as usual."
)

// handleRetrospective generates Structure Proposals now; they reach managers by DM and are listed here.
func handleRetrospective(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("retrospective denied user=%s", cmd.UserID)
		return
	}
	if proposals == nil || !cfg.LLMConfigured() {
		postEphemeral(api, cmd, noLLMText)
		return
	}
	postEphemeral(api, cmd, "Looking for structure changes (this can take a minute)…")
	ctx, cancel := context.WithTimeout(context.Background(), retrospectTimeout)
	defer cancel()
	ps, err := proposals.ProposeStructure(ctx)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Structure Proposals failed: %v", err))
		return
	}
	tax, _ := LoadTaxonomy(db)
	postEphemeral(api, cmd, retrospectText(tax, ps))
}

func retrospectText(tax domain.Taxonomy, ps []learning.StoredProposal) string {
	if len(ps) == 0 {
		return "*Structure Proposals:* none this time."
	}
	text := fmt.Sprintf("*Structure Proposals sent to managers (%d):*", len(ps))
	for _, p := range ps {
		text += "\n• " + proposalText(tax, p)
	}
	return text
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
		s = fmt.Sprintf("*New Section* “%s” under %s", esc(p.Name), esc(parent))
		if p.Description != "" {
			s += " — " + esc(p.Description)
		}
		s += fmt.Sprintf(". %d Work Items look like it.", p.Affected)
	case "split":
		names := make([]string, len(p.NewSections))
		for i, ns := range p.NewSections {
			names[i] = "“" + esc(ns.Name) + "”"
		}
		s = fmt.Sprintf("*Split* %s into %s. %d Open Work Items would be re-filed.", sectionName(tax, p.SectionID), strings.Join(names, ", "), p.Affected)
	case "retire":
		s = fmt.Sprintf("*Retire* %s: no new Work Items for 8 weeks and none open.", sectionName(tax, p.SectionID))
	}
	if p.Reason != "" {
		s += "\n_" + esc(p.Reason) + "_"
	}
	return s
}

func sectionName(tax domain.Taxonomy, id int64) string {
	if s, ok := tax.SectionAny(id); ok {
		return esc(s.Path())
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

func handleProposalDecision(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, value string, approve bool) {
	if !cfg.IsManagerID(cb.User.ID) || proposals == nil {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return
	}
	if !approve {
		switch err := proposals.RejectProposal(id, cb.User.ID); {
		case errors.Is(err, learning.ErrAlreadyDecided):
			replyToAction(api, cb, "This proposal was already decided by another manager.")
		case err != nil:
			replyToAction(api, cb, fmt.Sprintf("Reject failed: %v", err))
		default:
			replyToAction(api, cb, "Rejected. The same change won't be proposed again for 8 weeks.")
		}
		return
	}
	p, err := proposals.ApproveProposal(id, cb.User.ID)
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

func (n *LearningNotifier) ProposalsReady(ps []learning.StoredProposal) {
	tax, _ := LoadTaxonomy(n.DB)
	text, blocks := ProposalBlocks(tax, ps)
	for _, id := range n.managers() {
		dm(n.API, id, text, blocks)
	}
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
	fmt.Fprintf(&b, "- Filed automatically: reporter didn't answer: %d\n", st.FiledByFallback)
	if st.FailedFiling > 0 {
		fmt.Fprintf(&b, "- Filing failed (model unreachable): %d\n", st.FailedFiling)
	}
	fmt.Fprintf(&b, "- Undetermined on the report now: %d\n", st.Undetermined)
	if !llmOn {
		b.WriteString("_Structure Proposals are off: no LLM configured._\n")
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
				name = esc(s.Path())
			}
			fmt.Fprintf(&b, "- %s: %d\n", name, r.n)
		}
	}
	return b.String()
}
