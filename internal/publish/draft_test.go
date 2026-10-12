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

func TestNextDraftTimeIsFriday0900(t *testing.T) {
	loc := time.FixedZone("PDT", -7*3600)
	cases := map[string]string{
		"2026-10-07 15:00": "2026-10-09 09:00", // Wednesday -> this Friday
		"2026-10-09 08:59": "2026-10-09 09:00", // Friday before 09:00
		"2026-10-09 09:00": "2026-10-16 09:00", // at 09:00 -> next week
		"2026-10-10 10:00": "2026-10-16 09:00", // Saturday
	}
	for now, want := range cases {
		n, _ := time.ParseInLocation("2006-01-02 15:04", now, loc)
		if got := NextDraftTime(n, loc).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%s: got %s want %s", now, got, want)
		}
	}
}

func TestSaveDraftSnapshotsTheWeekWithoutPublishing(t *testing.T) {
	db, err := sqlite.InitDB(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids, _ := sqlite.SeedTaxonomy(db, []sqlite.SeedArea{{Name: "Product A", Categories: []sqlite.SeedCategory{{Name: "Infrastructure"}}}})
	loc := time.UTC
	friday := time.Date(2026, 9, 11, 9, 0, 0, 0, loc)
	sqlite.InsertWorkItem(db, domain.WorkItem{Description: "rack power", Author: "dev a", Source: "slack", Status: domain.StatusInProgress,
		ReportedAt: friday.Add(-48 * time.Hour), SectionID: ids[sqlite.SeedKey("Product A", "Infrastructure", "")], FilingState: domain.FilingFiled})
	out := t.TempDir()
	cfg := config.Config{ReportOutputDir: out, TeamName: "Team", Location: loc}

	if err := SaveDraft(db, cfg, friday); err != nil {
		t.Fatal(err)
	}
	d, ok, err := sqlite.GetReportDraft(db, "2026-09-07")
	if err != nil || !ok || !strings.Contains(d.TeamMarkdown, "- **Dev A** - Rack power (in progress)") || d.BossMarkdown == "" || d.ItemCount != 1 {
		t.Fatalf("draft: %+v ok=%v err=%v", d, ok, err)
	}
	if _, ok, _ := sqlite.GetPublishedReport(db, "2026-09-07"); ok {
		t.Fatal("a draft does not publish")
	}
	if files, _ := os.ReadDir(out); len(files) != 0 {
		t.Fatalf("a draft writes no report files, got %d", len(files))
	}
}
