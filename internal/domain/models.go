package domain

import (
	"fmt"
	"reportbot/internal/config"
	"time"
)

// Statuses in report order; done is terminal.
const (
	StatusInProgress = "in progress"
	StatusInTesting  = "in testing"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
)

var Statuses = []string{StatusInProgress, StatusInTesting, StatusBlocked, StatusDone}

// StatusRank orders statuses for rendering; unknown values sort last.
func StatusRank(status string) int {
	for i, s := range Statuses {
		if s == status {
			return i
		}
	}
	return len(Statuses)
}

func IsStatus(status string) bool { return StatusRank(status) < len(Statuses) }

const (
	PlacedByModel  = "model"
	PlacedByPerson = "person"
)

// Filing states of a Work Item's Placement.
const (
	FilingPending = "pending" // waiting for the models
	FilingFiled   = "filed"   // has a Section, or is Undetermined by a person's or model's decision
	FilingAsking  = "asking"  // Undetermined; the reporter was asked "Where does this go?"
	FilingFailed  = "failed"  // Undetermined; a model was unreachable after retries
)

// Reasons a Work Item sits in Undetermined.
const (
	ReasonLowConfidence    = "low_confidence"
	ReasonLLMUndetermined  = "llm_undetermined"
	ReasonModelUnreachable = "model_unreachable"
	ReasonSectionRetired   = "section_retired"
	ReasonNoTaxonomy       = "no_taxonomy"
)

type WorkItem struct {
	ID          int64
	Description string
	Author      string
	AuthorID    string // Slack user ID (immutable, for authorization)
	Source      string // "slack", "gitlab", "github" or "import"
	Status      string
	StatusNote  string
	TicketIDs   string // comma-separated: "1247202,1230118"
	ReportedAt  time.Time
	CreatedAt   time.Time
	DoneAt      time.Time // zero while not done

	SectionID          int64 // 0 = Undetermined
	PlacedBy           string
	Confidence         *float64 // Section check probability; nil when no check was made
	FilingState        string
	UndeterminedReason string
}

func (w WorkItem) IsOpen() bool { return w.Status != StatusDone }

// CodeChange is a merge or pull request; it is its own Work Item or linked to one.
type CodeChange struct {
	ID           int64
	Source       string // "gitlab" | "github"
	SourceRef    string // MR/PR URL
	Title        string
	Author       string
	AuthorID     string
	TicketIDs    string
	State        string // "open" | "merged" | "closed"
	MergedAt     time.Time
	WorkItemID   int64
	LinkKind     string // "own" | "linked" | "suggested"
	SuggestedFor int64
}

const (
	LinkOwn       = "own"
	LinkLinked    = "linked"
	LinkSuggested = "suggested"
)

// PlacedItem is a Work Item with a Placement, used as a similar-item example.
type PlacedItem struct {
	WorkItemID  int64
	Description string
	Author      string
	SectionID   int64
	ByPerson    bool
	ReportedAt  time.Time
}

type GitLabMR struct {
	Title       string
	Author      string // username
	AuthorName  string // display name
	AuthorID    int64  // immutable numeric account ID; the only thing that binds an owner
	WebURL      string
	TicketIDs   string // comma-separated ticket IDs parsed from configured MR field
	MergedAt    time.Time
	UpdatedAt   time.Time
	CreatedAt   time.Time
	State       string
	Labels      []string
	ProjectPath string
}

type GitHubPR struct {
	Title        string
	Author       string // GitHub login (username)
	AuthorID     int64  // immutable numeric account ID; the only thing that binds an owner
	AuthorName   string // same as Author (Search API doesn't return display name)
	HTMLURL      string // PR web URL, used as source_ref for dedup
	MergedAt     time.Time
	UpdatedAt    time.Time
	CreatedAt    time.Time
	ClosedAt     time.Time
	State        string // "open", "merged" (derived), or "closed"
	Body         string // PR description
	Labels       []string
	RepoFullName string // e.g. "org/repo-name"
}

func CurrentWeekRangeAt(now time.Time) (time.Time, time.Time) {
	weekday := now.Weekday()
	if weekday == time.Sunday {
		weekday = 7
	}
	daysFromMonday := int(weekday) - int(time.Monday)
	monday := time.Date(now.Year(), now.Month(), now.Day()-daysFromMonday, 0, 0, 0, 0, now.Location())
	nextMonday := monday.AddDate(0, 0, 7)
	return monday, nextMonday
}

// FridayOfWeek returns the Friday of the same week as the given Monday.
func FridayOfWeek(monday time.Time) time.Time {
	return monday.AddDate(0, 0, 4)
}

func ReportWeekRange(cfg config.Config, now time.Time) (time.Time, time.Time) {
	hour, min, err := parseClock(cfg.MondayCutoffTime)
	if err != nil {
		return CurrentWeekRangeAt(now)
	}

	if now.Weekday() == time.Monday {
		cutoff := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
		if now.Before(cutoff) {
			return CurrentWeekRangeAt(now.AddDate(0, 0, -7))
		}
	}
	return CurrentWeekRangeAt(now)
}

func parseClock(s string) (int, int, error) {
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
