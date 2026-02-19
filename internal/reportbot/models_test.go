package reportbot

import (
	"testing"
	"time"
)

func TestReportWeekRangeMondayCutoff(t *testing.T) {
	loc := time.FixedZone("UTC+0", 0)
	cfg := Config{MondayCutoffTime: "12:00"}

	mondayMorning := time.Date(2026, 2, 9, 9, 0, 0, 0, loc)
	from, to := ReportWeekRange(cfg, mondayMorning)
	if from.Format("20060102") != "20260202" || to.Format("20060102") != "20260209" {
		t.Fatalf("expected previous week for Monday morning, got %s -> %s", from.Format("20060102"), to.Format("20060102"))
	}

	mondayAfternoon := time.Date(2026, 2, 9, 13, 0, 0, 0, loc)
	from, to = ReportWeekRange(cfg, mondayAfternoon)
	if from.Format("20060102") != "20260209" || to.Format("20060102") != "20260216" {
		t.Fatalf("expected current week for Monday afternoon, got %s -> %s", from.Format("20060102"), to.Format("20060102"))
	}
}

func TestFridayOfWeek(t *testing.T) {
	loc := time.FixedZone("UTC+0", 0)

	tests := []struct {
		name     string
		monday   time.Time
		expected string
	}{
		{
			name:     "basic monday to friday",
			monday:   time.Date(2026, 2, 9, 0, 0, 0, 0, loc),
			expected: "20260213", // Feb 13, 2026 (Friday)
		},
		{
			name:     "monday with time component",
			monday:   time.Date(2026, 2, 9, 14, 30, 45, 0, loc),
			expected: "20260213", // Feb 13, 2026 (Friday)
		},
		{
			name:     "year boundary - monday in december",
			monday:   time.Date(2025, 12, 29, 0, 0, 0, 0, loc),
			expected: "20260102", // Jan 2, 2026 (Friday)
		},
		{
			name:     "year boundary - monday in january",
			monday:   time.Date(2026, 1, 5, 0, 0, 0, 0, loc),
			expected: "20260109", // Jan 9, 2026 (Friday)
		},
		{
			name:     "month boundary",
			monday:   time.Date(2026, 2, 23, 0, 0, 0, 0, loc),
			expected: "20260227", // Feb 27, 2026 (Friday)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			friday := FridayOfWeek(tt.monday)
			got := friday.Format("20060102")
			if got != tt.expected {
				t.Errorf("FridayOfWeek(%s) = %s, want %s",
					tt.monday.Format("20060102 15:04:05"), got, tt.expected)
			}

			// Verify it's actually a Friday
			if friday.Weekday() != time.Friday {
				t.Errorf("FridayOfWeek(%s) returned %s (weekday: %s), expected Friday",
					tt.monday.Format("20060102"), friday.Format("20060102"), friday.Weekday())
			}

			// Verify time components are preserved
			if friday.Hour() != tt.monday.Hour() || friday.Minute() != tt.monday.Minute() || friday.Second() != tt.monday.Second() {
				t.Errorf("FridayOfWeek(%s) time component not preserved: got %02d:%02d:%02d, want %02d:%02d:%02d",
					tt.monday.Format("20060102 15:04:05"),
					friday.Hour(), friday.Minute(), friday.Second(),
					tt.monday.Hour(), tt.monday.Minute(), tt.monday.Second())
			}

			// Verify location is preserved
			if friday.Location() != tt.monday.Location() {
				t.Errorf("FridayOfWeek(%s) location not preserved: got %v, want %v",
					tt.monday.Format("20060102"), friday.Location(), tt.monday.Location())
			}
		})
	}
}

func TestFridayOfWeekWithDifferentTimezones(t *testing.T) {
	utc := time.UTC
	pst := time.FixedZone("PST", -8*3600)
	jst := time.FixedZone("JST", 9*3600)

	tests := []struct {
		name     string
		monday   time.Time
		expected string
	}{
		{
			name:     "UTC timezone",
			monday:   time.Date(2026, 2, 9, 10, 0, 0, 0, utc),
			expected: "20260213",
		},
		{
			name:     "PST timezone",
			monday:   time.Date(2026, 2, 9, 10, 0, 0, 0, pst),
			expected: "20260213",
		},
		{
			name:     "JST timezone",
			monday:   time.Date(2026, 2, 9, 10, 0, 0, 0, jst),
			expected: "20260213",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			friday := FridayOfWeek(tt.monday)
			got := friday.Format("20060102")
			if got != tt.expected {
				t.Errorf("FridayOfWeek(%s) = %s, want %s",
					tt.monday.Format("20060102 15:04:05 MST"), got, tt.expected)
			}

			// Verify location is preserved
			if friday.Location() != tt.monday.Location() {
				t.Errorf("FridayOfWeek(%s) location not preserved: got %v, want %v",
					tt.monday.Format("20060102 15:04:05 MST"), friday.Location(), tt.monday.Location())
			}
		})
	}
}
