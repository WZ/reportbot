package filing

import (
	"fmt"
	"strconv"

	"reportbot/internal/domain"
	"reportbot/internal/integrations/decision"
)

// Question IDs and texts. Instructions are plain text that reference state by path.
const (
	QSectionCheck = "section_check"
	QStatus       = "status"
	QTicket       = "ticket"
	qUpdatePrefix = "update_w"
	qLinkPrefix   = "link_w"

	SectionCheckInstructions = "Does `work_item` belong in `proposed_section` of the team's weekly status report? " +
		"`similar_past_items` and `same_author_recent_items` show where the team filed earlier items."
	statusInstructions = "Which status of `work_item` does `status_text` describe? When `status_text` is empty, judge from the description."
	ticketInstructions = "Which of `ticket_candidates` is the issue-tracker ticket ID for `work_item`? " +
		"Version, build and merge request numbers are not ticket IDs."
	updateInstructions = "Is `work_item` a new report about the same work as `existing_items.%s`, rather than separate work?"
	linkInstructions   = "Is the merge request in `work_item` part of the work described by `existing_items.%s`?"
)

// statusCriteria keys are option names; StatusFromChoice maps them back.
var statusCriteria = map[string]string{
	"in_progress": "Work has started and is ongoing, including investigation, analysis, review or waiting on a reply.",
	"in_testing":  "The change is written and is being tested or verified.",
	"blocked":     "Work cannot continue until something outside the developer's control happens.",
	"done":        "The work is finished, delivered, merged or resolved.",
}

func StatusFromChoice(choice string) (string, bool) {
	switch choice {
	case "in_progress":
		return domain.StatusInProgress, true
	case "in_testing":
		return domain.StatusInTesting, true
	case "blocked":
		return domain.StatusBlocked, true
	case "done":
		return domain.StatusDone, true
	}
	return "", false
}

// FiledLine is a past Work Item and where it was filed, as the decision model sees it.
type FiledLine struct {
	Item       string `json:"item"`
	FiledUnder string `json:"filed_under"`
}

type stateWorkItem struct {
	Description string `json:"description"`
	Author      string `json:"author"`
	Source      string `json:"source"`
}

type stateSection struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type stateTicket struct {
	Number  string `json:"number"`
	Context string `json:"context"`
}

type stateExisting struct {
	Description string `json:"description"`
	Author      string `json:"author"`
	Status      string `json:"status"`
	Section     string `json:"section"`
	TicketIDs   string `json:"ticket_ids,omitempty"`
}

// Candidate is an Open Work Item the new report may be an Update to (or a Code Change may belong to).
type Candidate struct {
	Item        domain.WorkItem
	SectionPath string
}

// RequestInput is everything one decision-model request asks about a Work Item.
type RequestInput struct {
	Item         domain.WorkItem
	Proposed     *domain.Section // nil: no Section check
	Similar      []FiledLine
	AuthorRecent []FiledLine
	AskStatus    bool
	StatusText   string
	Tickets      []TicketCandidate // asked when non-empty
	Updates      []Candidate
	Links        []Candidate // Code Change → Open Work Item
}

func sourceText(source string) string {
	switch source {
	case "gitlab", "github":
		return "a merge request fetched from code hosting"
	default:
		return "reported by the developer in Slack"
	}
}

func UpdateQuestionID(itemID int64) string { return qUpdatePrefix + strconv.FormatInt(itemID, 10) }
func LinkQuestionID(itemID int64) string   { return qLinkPrefix + strconv.FormatInt(itemID, 10) }
func ticketKey(i int) string               { return "t" + strconv.Itoa(i+1) }

// BuildRequest returns the state and questions for one Work Item; questions may be empty.
func BuildRequest(in RequestInput) (map[string]any, map[string]decision.Question) {
	state := map[string]any{
		"work_item": stateWorkItem{Description: in.Item.Description, Author: in.Item.Author, Source: sourceText(in.Item.Source)},
	}
	qs := map[string]decision.Question{}

	if in.Proposed != nil {
		state["proposed_section"] = stateSection{Name: in.Proposed.Path(), Description: in.Proposed.Description}
		state["similar_past_items"] = nonNil(in.Similar)
		state["same_author_recent_items"] = nonNil(in.AuthorRecent)
		qs[QSectionCheck] = decision.Question{Type: "noul", Instructions: SectionCheckInstructions}
	}
	if in.AskStatus {
		state["status_text"] = in.StatusText
		qs[QStatus] = decision.Question{Type: "choice", Instructions: statusInstructions, Criteria: statusCriteria}
	}
	if len(in.Tickets) > 0 {
		cands := map[string]stateTicket{}
		criteria := map[string]string{"none": "None of the candidates is a ticket ID."}
		for i, t := range in.Tickets {
			cands[ticketKey(i)] = stateTicket{Number: t.Number, Context: t.Context}
			criteria[ticketKey(i)] = "`ticket_candidates." + ticketKey(i) + "`"
		}
		state["ticket_candidates"] = cands
		qs[QTicket] = decision.Question{Type: "choice", Instructions: ticketInstructions, Criteria: criteria}
	}
	existing := map[string]stateExisting{}
	add := func(c Candidate, qid, text string) {
		key := "w" + strconv.FormatInt(c.Item.ID, 10)
		existing[key] = stateExisting{Description: c.Item.Description, Author: c.Item.Author, Status: c.Item.Status,
			Section: c.SectionPath, TicketIDs: c.Item.TicketIDs}
		qs[qid] = decision.Question{Type: "noul", Instructions: fmt.Sprintf(text, key)}
	}
	for _, c := range in.Updates {
		add(c, UpdateQuestionID(c.Item.ID), updateInstructions)
	}
	for _, c := range in.Links {
		add(c, LinkQuestionID(c.Item.ID), linkInstructions)
	}
	if len(existing) > 0 {
		state["existing_items"] = existing
	}
	return state, qs
}

// TicketFromChoice maps the ticket answer back to a number; "" for none.
func TicketFromChoice(choice string, cands []TicketCandidate) string {
	for i, c := range cands {
		if ticketKey(i) == choice {
			return c.Number
		}
	}
	return ""
}

func nonNil(l []FiledLine) []FiledLine {
	if l == nil {
		return []FiledLine{}
	}
	return l
}
