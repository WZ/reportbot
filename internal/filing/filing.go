// Package filing files Work Items into the Taxonomy as they arrive: the LLM picks a
// Section, then one decision-model request checks it and answers the narrow questions.
package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
	"reportbot/internal/integrations/llm"
	"reportbot/internal/storage/sqlite"
)

const (
	similarForCheck    = 30
	recentPerSection   = 30
	recentWindow       = 28 * 24 * time.Hour
	authorRecentCount  = 10
	placedIndexLimit   = 5000
	correctionsWindow  = 28 * 24 * time.Hour
	correctionsLimit   = 20
	updateAuthorTopK   = 3
	updateTeamTopK     = 2
	updateThreshold    = 0.5
	defaultExampleSize = 20
)

// Classifier picks Sections with the LLM (llm.ClassifySections in production).
type Classifier func(cfg config.Config, sections []domain.Section, recent map[int64][]llm.Example, items []llm.PromptItem, corrections []llm.CorrectionLine) (llm.SectionResult, error)

// Asker sends one decision-model request (*decision.Client in production).
type Asker interface {
	Ask(ctx context.Context, state any, questions map[string]decision.Question) (*decision.Response, error)
}

// Why a Work Item is being filed.
const (
	KindArrival    = "arrival"     // a new /report
	KindEdit       = "edit"        // its text changed
	KindCodeChange = "code_change" // a newly fetched Code Change
	KindRetry      = "retry"       // the sweep, after a failure or a stall
)

// Payload travels with a filing job: where to reply and why the item is being filed.
type Payload struct {
	Kind        string `json:"kind"`
	ResponseURL string `json:"response_url,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Replace     bool   `json:"replace,omitempty"` // the reply replaces the "Saved" message
}

func (p Payload) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

func ParsePayload(s string) Payload {
	var p Payload
	_ = json.Unmarshal([]byte(s), &p)
	return p
}

// Result is what the reporter is told after a filing pass.
type Result struct {
	Item    domain.WorkItem
	Section *domain.Section // nil when Undetermined
	Payload Payload
	// Choices for "Where does this go?": the LLM's Section first, then the top Sections by similar-item vote.
	Choices []domain.Section
	// Kept is the Section an edited item stays in while the reporter decides.
	Kept   *domain.Section
	Update *Candidate // likely Update target, the reporter confirms
	Link   *Candidate // likely Work Item for a Code Change, the author confirms
	Failed bool       // a model was unreachable after retries
}

// Notifier tells people what filing decided (Slack in production).
type Notifier interface {
	Filed(r Result)
	PlacementOverridden(item domain.WorkItem, eventID int64, from, to domain.Section)
}

type Filer struct {
	DB       *sql.DB
	Cfg      config.Config
	Classify Classifier
	Decision Asker
	Notify   Notifier
	Now      func() time.Time
}

func (f *Filer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Filer) exampleCount() int {
	if f.Cfg.LLMExampleCount > 0 {
		return f.Cfg.LLMExampleCount
	}
	return defaultExampleSize
}

// context is everything a filing pass needs that does not depend on the item.
type filingContext struct {
	tax         domain.Taxonomy
	sections    []domain.Section
	active      map[int64]domain.Section
	index       *llm.SimilarIndex
	placed      []domain.PlacedItem
	recent      map[int64][]llm.Example // items filed in each Section lately, like the current report's lines
	corrections []llm.CorrectionLine
}

func (f *Filer) loadContext() (*filingContext, error) {
	tax, err := sqlite.LoadTaxonomy(f.DB)
	if err != nil {
		return nil, err
	}
	c := &filingContext{tax: tax, sections: tax.Sections(), active: map[int64]domain.Section{}}
	for _, s := range c.sections {
		c.active[s.ID] = s
	}
	placed, err := sqlite.GetPlacedItems(f.DB, placedIndexLimit)
	if err != nil {
		return nil, err
	}
	for _, p := range placed {
		if _, ok := c.active[p.SectionID]; ok {
			c.placed = append(c.placed, p)
		}
	}
	c.index = llm.NewSimilarIndex(c.placed)
	c.recent = map[int64][]llm.Example{}
	since := f.now().Add(-recentWindow)
	for _, p := range c.placed { // newest first
		if p.ReportedAt.After(since) && len(c.recent[p.SectionID]) < recentPerSection {
			c.recent[p.SectionID] = append(c.recent[p.SectionID], llm.Example{SectionID: p.SectionID, Author: p.Author, Description: p.Description})
		}
	}
	corr, err := sqlite.GetRecentCorrections(f.DB, f.now().Add(-correctionsWindow), correctionsLimit)
	if err != nil {
		return nil, err
	}
	for _, x := range corr {
		_, okFrom := c.active[x.FromSection]
		_, okTo := c.active[x.ToSection]
		if okFrom && okTo {
			c.corrections = append(c.corrections, llm.CorrectionLine{Description: x.Description, From: x.FromSection, To: x.ToSection})
		}
	}
	return c, nil
}

func (c *filingContext) similar(item domain.WorkItem, k int) []domain.PlacedItem {
	return c.index.TopK(item.Description, k, func(p domain.PlacedItem) bool { return p.WorkItemID != item.ID })
}

func (c *filingContext) filedLines(items []domain.PlacedItem) []FiledLine {
	out := make([]FiledLine, 0, len(items))
	for _, p := range items {
		out = append(out, FiledLine{Item: p.Author + ": " + p.Description, FiledUnder: c.active[p.SectionID].Path()})
	}
	return out
}

func (c *filingContext) authorRecent(item domain.WorkItem) []domain.PlacedItem {
	var out []domain.PlacedItem
	for _, p := range c.placed { // newest first
		if p.WorkItemID != item.ID && strings.EqualFold(strings.TrimSpace(p.Author), strings.TrimSpace(item.Author)) {
			out = append(out, p)
			if len(out) == authorRecentCount {
				break
			}
		}
	}
	return out
}

func (f *Filer) promptItem(c *filingContext, item domain.WorkItem, similar []domain.PlacedItem) llm.PromptItem {
	n := min(f.exampleCount(), len(similar))
	ex := make([]llm.Example, 0, n)
	for _, p := range similar[:n] {
		ex = append(ex, llm.Example{SectionID: p.SectionID, Author: p.Author, Description: p.Description})
	}
	return llm.PromptItem{ID: item.ID, Author: item.Author, Source: item.Source, Text: item.Description, Similar: ex}
}

// voteChoices returns the LLM's pick (if any) and then the Sections most common among
// the similar items, up to three in all.
func (c *filingContext) voteChoices(pick int64, similar []domain.PlacedItem, examples int) []domain.Section {
	var out []domain.Section
	seen := map[int64]bool{}
	if s, ok := c.active[pick]; ok {
		out = append(out, s)
		seen[pick] = true
	}
	counts := map[int64]int{}
	var order []int64
	for _, p := range similar[:min(examples, len(similar))] {
		if counts[p.SectionID] == 0 {
			order = append(order, p.SectionID)
		}
		counts[p.SectionID]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	for _, id := range order {
		if len(out) == 3 {
			break
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, c.active[id])
		}
	}
	return out
}

func judgmentJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// FileItem runs one filing pass. An error means a model was unreachable or the DB failed;
// the job is retried.
func (f *Filer) FileItem(ctx context.Context, itemID int64, payload Payload) error {
	item, err := sqlite.GetWorkItemByID(f.DB, itemID)
	if err == sql.ErrNoRows {
		return nil // deleted (e.g. merged as an Update) before filing
	}
	if err != nil {
		return err
	}
	c, err := f.loadContext()
	if err != nil {
		return err
	}
	if len(c.sections) == 0 {
		out := sqlite.FilingOutcome{Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, FilingState: domain.FilingFiled,
			Reason: domain.ReasonNoTaxonomy, HistoryNote: sqlite.ReasonArrival}}
		if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
			return err
		}
		item, _ = sqlite.GetWorkItemByID(f.DB, itemID)
		f.notify(Result{Item: item, Payload: payload})
		return nil
	}

	similar := c.similar(item, max(similarForCheck, f.exampleCount()))
	res, err := f.Classify(f.Cfg, c.sections, c.recent, []llm.PromptItem{f.promptItem(c, item, similar)}, c.corrections)
	if err != nil {
		return fmt.Errorf("llm: %w", err)
	}
	pick := res.Picks[item.ID]
	judgments := []sqlite.Judgment{{
		Kind: "section_pick", Subject: llm.SectionKey(pick), Model: res.Model,
		AnswerJSON:  judgmentJSON(map[string]any{"section_id": pickKey(pick), "raw": res.RawText}),
		InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens,
	}}

	in := RequestInput{Item: item}
	if s, ok := c.active[pick]; ok {
		in.Proposed = &s
		in.Similar = c.filedLines(similar[:min(similarForCheck, len(similar))])
		in.AuthorRecent = c.filedLines(c.authorRecent(item))
	}
	if item.Status == "" {
		in.AskStatus, in.StatusText = true, item.StatusNote
	}
	if item.TicketIDs == "" && item.Source == "slack" {
		if _, cands := FindTickets(item.Description); len(cands) > 0 {
			in.Tickets = cands
		}
	}
	// Update and Link candidates are for a new Work Item's first filing only.
	first := payload.Kind == KindArrival || payload.Kind == KindCodeChange
	if payload.Kind == KindRetry {
		filed, err := sqlite.HasBeenFiled(f.DB, item.ID)
		if err != nil {
			return err
		}
		first = !filed
	}
	var codeChange *domain.CodeChange
	switch item.Source {
	case "slack":
		if first {
			in.Updates, err = f.updateCandidates(c, item)
		}
	case "gitlab", "github":
		if first {
			if cc, ok := f.ownCodeChange(item.ID); ok {
				codeChange = &cc
				in.Links, err = f.updateCandidates(c, item)
			}
		}
	}
	if err != nil {
		return err
	}

	state, qs := BuildRequest(in)
	var ans *decision.Response
	if len(qs) > 0 {
		ans, err = f.Decision.Ask(ctx, state, qs)
		if err != nil {
			return fmt.Errorf("decision model: %w", err)
		}
		ids := make([]string, 0, len(ans.Answers))
		for id := range ans.Answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for i, id := range ids {
			j := sqlite.Judgment{Kind: judgmentKind(id), Subject: id, AnswerJSON: string(ans.Answers[id].Raw),
				Model: ans.Model, LatencyMS: ans.Latency.Milliseconds()}
			if i == 0 { // usage is per request; record it once
				j.InputTokens, j.OutputTokens = ans.Usage.InputTokens, ans.Usage.OutputTokens
			}
			judgments = append(judgments, j)
		}
	}

	out := sqlite.FilingOutcome{Judgments: judgments}
	out.Placement = sqlite.Placement{PlacedBy: domain.PlacedByModel, LLMSectionID: pick, HistoryNote: sqlite.ReasonArrival}
	result := Result{Payload: payload}
	switch {
	case in.Proposed == nil:
		out.Placement.FilingState, out.Placement.Reason = domain.FilingAsking, domain.ReasonLLMUndetermined
	default:
		p := ans.Answers[QSectionCheck].Noul
		out.Placement.Confidence = &p
		if p >= f.Cfg.DecisionModelConfidence {
			out.Placement.SectionID, out.Placement.FilingState = pick, domain.FilingFiled
			s := c.active[pick]
			result.Section = &s
		} else {
			out.Placement.FilingState, out.Placement.Reason = domain.FilingAsking, domain.ReasonLowConfidence
		}
	}
	if result.Section == nil {
		result.Choices = c.voteChoices(pick, similar, f.exampleCount())
		if cur, ok := c.active[item.SectionID]; ok && !first {
			// An unsure re-file keeps the Section the item has, and still asks the reporter.
			out.Placement = sqlite.Placement{SectionID: cur.ID, PlacedBy: item.PlacedBy, Confidence: item.Confidence,
				LLMSectionID: pick, FilingState: domain.FilingFiled, HistoryNote: sqlite.ReasonArrival}
			result.Kept = &cur
		}
	}
	if in.AskStatus {
		if st, ok := StatusFromChoice(ans.Answers[QStatus].Choice); ok {
			out.Status, out.StatusNote = st, item.StatusNote
		}
	}
	if len(in.Tickets) > 0 {
		out.TicketIDs = TicketFromChoice(ans.Answers[QTicket].Choice, in.Tickets)
	}
	result.Update = bestCandidate(ans, in.Updates, UpdateQuestionID)
	result.Link = bestCandidate(ans, in.Links, LinkQuestionID)

	if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
		return err
	}
	if codeChange != nil && result.Link != nil {
		if err := sqlite.SetCodeChangeSuggestion(f.DB, codeChange.ID, result.Link.Item.ID); err != nil {
			return err
		}
	}
	if result.Item, err = sqlite.GetWorkItemByID(f.DB, itemID); err != nil {
		return err
	}
	f.notify(result)
	return nil
}

func pickKey(pick int64) string {
	if pick == 0 {
		return llm.UndeterminedID
	}
	return llm.SectionKey(pick)
}

func judgmentKind(questionID string) string {
	switch {
	case questionID == QSectionCheck, questionID == QStatus, questionID == QTicket:
		return questionID
	case strings.HasPrefix(questionID, qUpdatePrefix):
		return "update_candidate"
	case strings.HasPrefix(questionID, qLinkPrefix):
		return "link_candidate"
	}
	return questionID
}

func bestCandidate(ans *decision.Response, cands []Candidate, qid func(int64) string) *Candidate {
	if ans == nil {
		return nil
	}
	var best *Candidate
	bestP := updateThreshold
	for i := range cands {
		if p := ans.Answers[qid(cands[i].Item.ID)].Noul; p >= bestP {
			best, bestP = &cands[i], p
		}
	}
	return best
}

func (f *Filer) ownCodeChange(itemID int64) (domain.CodeChange, bool) {
	ccs, err := sqlite.GetCodeChangesForItem(f.DB, itemID)
	if err != nil {
		return domain.CodeChange{}, false
	}
	for _, cc := range ccs {
		if cc.LinkKind == domain.LinkOwn {
			return cc, true
		}
	}
	return domain.CodeChange{}, false
}

// updateCandidates proposes Open Work Items the new item may be about: exact ticket
// overlap, then the most similar of the same author's and the team's open items.
func (f *Filer) updateCandidates(c *filingContext, item domain.WorkItem) ([]Candidate, error) {
	open, err := sqlite.GetOpenItems(f.DB)
	if err != nil {
		return nil, err
	}
	byID := map[int64]domain.WorkItem{}
	var mine, team []domain.PlacedItem
	for _, o := range open {
		if o.ID == item.ID || o.FilingState == domain.FilingPending {
			continue
		}
		byID[o.ID] = o
		p := domain.PlacedItem{WorkItemID: o.ID, Description: o.Description, Author: o.Author, SectionID: o.SectionID}
		if sameAuthor(o, item) {
			mine = append(mine, p)
		} else {
			team = append(team, p)
		}
	}
	var ids []int64
	seen := map[int64]bool{}
	addID := func(id int64) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	certain, _ := FindTickets(item.Description)
	tickets := append(certain, splitTickets(item.TicketIDs)...)
	for _, o := range open {
		if _, ok := byID[o.ID]; ok && ticketsOverlap(tickets, splitTickets(o.TicketIDs)) {
			addID(o.ID)
		}
	}
	for _, p := range llm.NewSimilarIndex(mine).TopK(item.Description, updateAuthorTopK, nil) {
		addID(p.WorkItemID)
	}
	for _, p := range llm.NewSimilarIndex(team).TopK(item.Description, updateTeamTopK, nil) {
		addID(p.WorkItemID)
	}
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		o := byID[id]
		path := "Undetermined"
		if s, ok := c.tax.SectionAny(o.SectionID); ok {
			path = s.Path()
		}
		out = append(out, Candidate{Item: o, SectionPath: path})
	}
	return out, nil
}

func sameAuthor(a, b domain.WorkItem) bool {
	if a.AuthorID != "" && b.AuthorID != "" {
		return a.AuthorID == b.AuthorID
	}
	return strings.EqualFold(strings.TrimSpace(a.Author), strings.TrimSpace(b.Author))
}

func splitTickets(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func ticketsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// MarkFailed files an item as Undetermined because a model stayed unreachable.
func (f *Filer) MarkFailed(itemID int64, payload Payload) error {
	out := sqlite.FilingOutcome{Placement: sqlite.Placement{PlacedBy: domain.PlacedByModel, FilingState: domain.FilingFailed,
		Reason: domain.ReasonModelUnreachable, HistoryNote: sqlite.ReasonArrival}}
	item, err := sqlite.GetWorkItemByID(f.DB, itemID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if item.SectionID != 0 { // keep a Section it already has (e.g. a re-file after an edit)
		out.Placement.SectionID, out.Placement.PlacedBy, out.Placement.Confidence = item.SectionID, item.PlacedBy, item.Confidence
		out.Placement.Reason = ""
	}
	if err := sqlite.ApplyFiling(f.DB, itemID, out); err != nil {
		return err
	}
	// Tell the author once per Work Item, not on every sweep retry.
	first, err := sqlite.MarkFailureNotified(f.DB, itemID)
	if err != nil || !first {
		return err
	}
	item, err = sqlite.GetWorkItemByID(f.DB, itemID)
	if err != nil {
		return err
	}
	f.notify(Result{Item: item, Payload: payload, Failed: true})
	return nil
}

func (f *Filer) notify(r Result) {
	if f.Notify == nil {
		return
	}
	if r.Section == nil && r.Kept == nil && len(r.Choices) == 0 && r.Item.SectionID != 0 {
		if tax, err := sqlite.LoadTaxonomy(f.DB); err == nil {
			if s, ok := tax.SectionAny(r.Item.SectionID); ok {
				r.Section = &s
			}
		}
	}
	f.Notify.Filed(r)
}
