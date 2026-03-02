package nudge

import (
	"strings"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	h, m, err := parseTime("10:05")
	if err != nil {
		t.Fatalf("parseTime returned error: %v", err)
	}
	if h != 10 || m != 5 {
		t.Fatalf("unexpected parseTime result: %02d:%02d", h, m)
	}

	if _, _, err := parseTime("24:00"); err == nil {
		t.Fatal("expected parseTime to fail for out-of-range hour")
	}
	if _, _, err := parseTime("foo"); err == nil {
		t.Fatal("expected parseTime to fail for malformed input")
	}
}

func TestNextWeekday(t *testing.T) {
	loc := time.UTC

	// Same day before target time -> same day trigger.
	now := time.Date(2026, 2, 20, 9, 0, 0, 0, loc) // Friday
	next := nextWeekday(now, time.Friday, 10, 0)
	want := time.Date(2026, 2, 20, 10, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("unexpected nextWeekday same-day result: got %v want %v", next, want)
	}

	// Same day after target time -> next week.
	now = time.Date(2026, 2, 20, 11, 0, 0, 0, loc) // Friday
	next = nextWeekday(now, time.Friday, 10, 0)
	want = time.Date(2026, 2, 27, 10, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("unexpected nextWeekday rollover result: got %v want %v", next, want)
	}

	// Different day -> nearest upcoming requested weekday.
	now = time.Date(2026, 2, 18, 12, 0, 0, 0, loc) // Wednesday
	next = nextWeekday(now, time.Friday, 10, 0)
	want = time.Date(2026, 2, 20, 10, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("unexpected nextWeekday cross-day result: got %v want %v", next, want)
	}
}

func TestFormatNudgeItem_AddsLineNumberAndTruncates(t *testing.T) {
	item := WorkItem{
		Description: "This is a very long work item description that should be truncated so the primary action can stay aligned with the text row in Slack.",
		Status:      "resolved in session; root cause analysis in progress",
		TicketIDs:   "7003001",
	}

	got := formatNudgeItem(3, item)
	if got[:3] != "3. " {
		t.Fatalf("expected line number prefix, got %q", got)
	}
	if !strings.Contains(got, "[7003001]") {
		t.Fatalf("expected ticket prefix, got %q", got)
	}
	if !strings.Contains(got, "_(current: resolved in session; root cause analysis in progress)_") {
		t.Fatalf("expected status suffix, got %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected truncated description, got %q", got)
	}
}
