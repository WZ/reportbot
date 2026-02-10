package main

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
