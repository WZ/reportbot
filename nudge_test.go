package main

import (
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
