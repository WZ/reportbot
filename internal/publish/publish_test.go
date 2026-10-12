package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

func TestPublishSnapshotsTheLiveReport(t *testing.T) {
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids, _ := sqlite.SeedTaxonomy(db, []sqlite.SeedArea{{Name: "Product A", Categories: []sqlite.SeedCategory{{Name: "Infrastructure"}}}})
	infra := ids[sqlite.SeedKey("Product A", "Infrastructure", "")]
	loc := time.UTC
	monday := time.Date(2026, 9, 7, 0, 0, 0, 0, loc)
	next := monday.AddDate(0, 0, 7)
	sqlite.InsertWorkItem(db, domain.WorkItem{Description: "rack power", Author: "dev a", Source: "slack", Status: domain.StatusInProgress,
		ReportedAt: monday.Add(time.Hour), SectionID: infra, FilingState: domain.FilingFiled})
	sqlite.InsertWorkItem(db, domain.WorkItem{Description: "unsure", Author: "dev b", Source: "slack", Status: domain.StatusInProgress,
		ReportedAt: monday.Add(time.Hour), FilingState: domain.FilingAsking})

	cfg := config.Config{ReportOutputDir: t.TempDir(), TeamName: "Team", Location: loc}
	live, err := LoadRange(db, monday, next)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Publish(db, cfg, live, "UMGR")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.TeamPath, "Team_20260911.md") || !strings.HasSuffix(res.BossPath, "Team_20260911.eml") {
		t.Fatalf("unexpected files: %s %s", res.TeamPath, res.BossPath)
	}
	md, _ := os.ReadFile(res.TeamPath)
	if !strings.Contains(string(md), "- **Dev A** - Rack power (in progress)") || !strings.Contains(string(md), "#### Undetermined") {
		t.Fatalf("unexpected team report:\n%s", md)
	}
	stored, ok, err := sqlite.GetPublishedReport(db, "2026-09-07")
	if err != nil || !ok || stored.TeamMarkdown != res.Team || stored.UndeterminedCount != 1 || stored.PublishedBy != "UMGR" {
		t.Fatalf("unexpected Published Report: %+v ok=%v err=%v", stored, ok, err)
	}

	// The Published Report does not change when live items change, until re-published.
	sqlite.SetWorkItemStatus(db, 1, domain.StatusDone, sqlite.Actor{Via: "test"})
	again, _, _ := sqlite.GetPublishedReport(db, "2026-09-07")
	if again.TeamMarkdown != stored.TeamMarkdown {
		t.Fatal("a Published Report is a snapshot")
	}
}
