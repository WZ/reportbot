package slackbot

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"

	"github.com/slack-go/slack"
)

// handleRetrospective asks the LLM for one-line Section descriptions that would have
// avoided recent corrections; a manager applies or dismisses each.
func handleRetrospective(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		log.Printf("retrospective denied user=%s", cmd.UserID)
		return
	}
	corrections, err := sqlite.GetRecentCorrections(db, time.Now().In(cfg.Location).AddDate(0, 0, -28), 200)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading corrections: %v", err))
		return
	}
	tax, err := LoadTaxonomy(db)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading the Taxonomy: %v", err))
		return
	}
	sections := tax.Sections()
	active := map[int64]bool{}
	for _, s := range sections {
		active[s.ID] = true
	}
	var lines []llm.CorrectionLine
	for _, c := range corrections {
		if active[c.FromSection] && active[c.ToSection] {
			lines = append(lines, llm.CorrectionLine{Description: c.Description, From: c.FromSection, To: c.ToSection})
		}
	}
	if len(lines) == 0 {
		postEphemeral(api, cmd, "No corrections found in the last 4 weeks.")
		return
	}
	postEphemeral(api, cmd, fmt.Sprintf("Analyzing %d corrections from the last 4 weeks...", len(lines)))

	suggestions, usage, err := llm.SuggestSectionDescriptions(cfg, sections, lines)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error analyzing corrections: %v", err))
		log.Printf("retrospective analysis error: %v", err)
		return
	}
	if len(suggestions) == 0 {
		postEphemeral(api, cmd, fmt.Sprintf("No Section needs a new description (tokens used: %s).", formatTokenCount(usage.TotalTokens())))
		return
	}
	for i, sg := range suggestions {
		sec, _ := tax.Section(sg.SectionID)
		text := fmt.Sprintf("*Suggestion %d: %s*\n%s\n_Description for %s:_ %s", i+1, sg.Title, sg.Reasoning, sec.Path(), sg.Description)
		var buttons []slack.BlockElement
		if value := fmt.Sprintf("%d|%s", sg.SectionID, sg.Description); len(value) <= 2000 {
			buttons = append(buttons, button(actionRetroApply, value, "Apply"))
		}
		buttons = append(buttons, button(actionRetroDismiss, strconv.Itoa(i), "Dismiss"))
		if _, err := api.PostEphemeral(cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false),
			slack.MsgOptionBlocks(mdSection(text), slack.NewActionBlock("", buttons...))); err != nil {
			log.Printf("retrospective message error suggestion=%d: %v", i, err)
		}
	}
	postEphemeral(api, cmd, fmt.Sprintf("Found %d suggestions (tokens used: %s).", len(suggestions), formatTokenCount(usage.TotalTokens())))
}

// handleRetroApply writes the suggested description to the Section; both models see it
// from the next filing on, and Open Work Items are re-classified.
func handleRetroApply(api *slack.Client, db *sql.DB, cfg Config, cb slack.InteractionCallback, act *slack.BlockAction) {
	if !cfg.IsManagerID(cb.User.ID) {
		return
	}
	idText, description, ok := strings.Cut(act.Value, "|")
	sectionID, err := strconv.ParseInt(idText, 10, 64)
	if !ok || err != nil || strings.TrimSpace(description) == "" {
		replyToAction(api, cb, "Invalid suggestion data.")
		return
	}
	if err := sqlite.SetSectionDescription(db, sectionID, description); err != nil {
		replyToAction(api, cb, fmt.Sprintf("Error applying the description: %v", err))
		return
	}
	if err := sqlite.EnqueueReclassify(db); err != nil {
		log.Printf("retrospective reclassify enqueue error: %v", err)
	}
	wakeFiling()
	replyToAction(api, cb, "Applied. Open items will be re-classified with the new description.")
	log.Printf("retrospective applied section=%d by=%s", sectionID, cb.User.ID)
}

// handleReportStats shows this week's filing: arrivals, auto-filed vs asked, corrections, Undetermined.
func handleReportStats(api *slack.Client, db *sql.DB, cfg Config, cmd slack.SlashCommand) {
	if !cfg.IsManagerID(cmd.UserID) {
		postEphemeral(api, cmd, "Sorry, only managers can use this command.")
		return
	}
	monday, nextMonday := ReportWeekRange(cfg, time.Now().In(cfg.Location))
	st, err := sqlite.GetWeekStats(db, monday, nextMonday)
	if err != nil {
		postEphemeral(api, cmd, fmt.Sprintf("Error loading stats: %v", err))
		return
	}
	tax, _ := LoadTaxonomy(db)
	postEphemeral(api, cmd, formatStats(st, tax, monday, nextMonday))
}

func formatStats(st sqlite.WeekStats, tax domain.Taxonomy, monday, nextMonday time.Time) string {
	pct := func(n int) string {
		if st.ItemsThisWeek == 0 {
			return "0%"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(st.ItemsThisWeek))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*Filing for %s - %s*\n", monday.Format("Jan 2"), nextMonday.AddDate(0, 0, -1).Format("Jan 2"))
	fmt.Fprintf(&b, "- Items reported or fetched: %d\n", st.ItemsThisWeek)
	fmt.Fprintf(&b, "- Filed automatically: %d (%s)\n", st.FiledByModel, pct(st.FiledByModel))
	fmt.Fprintf(&b, "- Asked \"Where does this go?\": %d (%s)\n", st.Asked, pct(st.Asked))
	if st.FailedFiling > 0 {
		fmt.Fprintf(&b, "- Filing failed (model unreachable): %d\n", st.FailedFiling)
	}
	fmt.Fprintf(&b, "- Undetermined on the report now: %d\n", st.Undetermined)
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
