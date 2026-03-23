package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/report"
	"reportbot/internal/web"
	mw "reportbot/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// --- Test helpers ---

// testConfig returns a minimal Config for tests.
func testConfig() web.Config {
	return web.Config{
		TeamName:         "TestTeam",
		ManagerSlackIDs:  []string{"UMGR"},
		WebSessionSecret: "test-secret-32-bytes-long-xxxxx",
	}
}

// authMiddleware returns a chi-compatible middleware that injects auth context.
func authMiddleware(userID, userName string, isManager bool) func(http.Handler) http.Handler {
	return mw.Auth(
		func(cookie *http.Cookie) (mw.SessionPayload, error) {
			return mw.SessionPayload{UserID: userID, UserName: userName}, nil
		},
		func() *http.Cookie {
			return &http.Cookie{Name: "reportbot_session", Value: "", Path: "/", MaxAge: -1}
		},
		func(uid string) bool {
			return isManager
		},
	)
}

// sessionCookie returns a dummy session cookie so the auth middleware doesn't reject for missing cookie.
func sessionCookie() *http.Cookie {
	return &http.Cookie{Name: "reportbot_session", Value: "test"}
}

// saveDeps saves and returns a restore function for all overrideable deps.
type depsSnapshot struct {
	GetItemsByDateRange          func(*sql.DB, time.Time, time.Time) ([]web.WorkItem, error)
	GetWorkItemByID              func(*sql.DB, int64) (web.WorkItem, error)
	GetRecentCorrections         func(*sql.DB, time.Time, int) ([]web.ClassificationCorrection, error)
	GetClassifiedItemsWithSections func(*sql.DB, time.Time, int) ([]domain.HistoricalItem, error)
	BuildReportsFromLast         func(web.Config, []web.WorkItem, time.Time, []web.ClassificationCorrection, []domain.HistoricalItem) (web.BuildResult, error)
	RenderMarkdownByMode         func(*web.ReportTemplate, string) string
	ReclassifyItem               func(*sql.DB, web.ClassificationCorrection, string) error
	UpdateWorkItemTextAndStatus  func(*sql.DB, int64, string, string) error
	DeleteWorkItemByID           func(*sql.DB, int64) error
	WriteReportFile              func(string, string, time.Time, string) (string, error)
	IsManagerID                  func(web.Config, string) bool
	ReportWeekRange              func(web.Config, time.Time) (time.Time, time.Time)
}

func saveDeps() (restore func()) {
	snap := depsSnapshot{
		GetItemsByDateRange:          web.GetItemsByDateRange,
		GetWorkItemByID:              web.GetWorkItemByID,
		GetRecentCorrections:         web.GetRecentCorrections,
		GetClassifiedItemsWithSections: web.GetClassifiedItemsWithSections,
		BuildReportsFromLast:         web.BuildReportsFromLast,
		RenderMarkdownByMode:         web.RenderMarkdownByMode,
		ReclassifyItem:               web.ReclassifyItem,
		UpdateWorkItemTextAndStatus:  web.UpdateWorkItemTextAndStatus,
		DeleteWorkItemByID:           web.DeleteWorkItemByID,
		WriteReportFile:              web.WriteReportFile,
		IsManagerID:                  web.IsManagerID,
		ReportWeekRange:              web.ReportWeekRange,
	}
	return func() {
		web.GetItemsByDateRange = snap.GetItemsByDateRange
		web.GetWorkItemByID = snap.GetWorkItemByID
		web.GetRecentCorrections = snap.GetRecentCorrections
		web.GetClassifiedItemsWithSections = snap.GetClassifiedItemsWithSections
		web.BuildReportsFromLast = snap.BuildReportsFromLast
		web.RenderMarkdownByMode = snap.RenderMarkdownByMode
		web.ReclassifyItem = snap.ReclassifyItem
		web.UpdateWorkItemTextAndStatus = snap.UpdateWorkItemTextAndStatus
		web.DeleteWorkItemByID = snap.DeleteWorkItemByID
		web.WriteReportFile = snap.WriteReportFile
		web.IsManagerID = snap.IsManagerID
		web.ReportWeekRange = snap.ReportWeekRange
	}
}

// stubEmptyBuild stubs out the build pipeline to return empty/no-op results.
func stubEmptyBuild() {
	web.GetRecentCorrections = func(db *sql.DB, since time.Time, limit int) ([]web.ClassificationCorrection, error) {
		return nil, nil
	}
	web.GetClassifiedItemsWithSections = func(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
		return nil, nil
	}
	web.BuildReportsFromLast = func(cfg web.Config, items []web.WorkItem, reportDate time.Time, corrections []web.ClassificationCorrection, historicalItems []domain.HistoricalItem) (web.BuildResult, error) {
		return web.BuildResult{
			Template: &report.ReportTemplate{
				Categories: []report.TemplateCategory{
					{
						Name: "Engineering",
						Subsections: []report.TemplateSubsection{
							{
								Name: "General",
								Items: []report.TemplateItem{
									{Author: "Alice", Description: "Fix login bug", Status: "done", IsNew: true},
								},
							},
						},
					},
				},
			},
			Decisions: map[int64]web.LLMSectionDecision{
				1: {SectionID: "S0_0", Confidence: 0.95},
			},
		}, nil
	}
	web.ReportWeekRange = func(cfg web.Config, now time.Time) (time.Time, time.Time) {
		monday := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
		return monday, monday.AddDate(0, 0, 7)
	}
}

// --- Tests ---

func TestEditorPage_LoadsItemsGroupedBySection(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return []web.WorkItem{
			{ID: 1, Description: "Fix login bug", Author: "Alice", AuthorID: "UMGR", Source: "slack", Status: "done"},
			{ID: 2, Description: "Add search feature", Author: "Bob", AuthorID: "UMGR", Source: "gitlab", Status: "in progress"},
		}, nil
	}
	stubEmptyBuild()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/", ReportEditorPage(cfg, nil))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Weekly Report") {
		t.Error("expected response to contain 'Weekly Report'")
	}
}

func TestEditorPage_EmptyWeek(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return nil, nil
	}
	stubEmptyBuild()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/", ReportEditorPage(cfg, nil))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	// With no items, ItemCount in the rendered template should be 0
	if strings.Contains(body, "Engineering") {
		t.Error("expected no sections to be rendered for an empty week")
	}
}

func TestEditorPage_WeekParameter(t *testing.T) {
	restore := saveDeps()
	defer restore()

	var capturedFrom, capturedTo time.Time
	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		capturedFrom = from
		capturedTo = to
		return nil, nil
	}
	stubEmptyBuild()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/", ReportEditorPage(cfg, nil))

	req := httptest.NewRequest("GET", "/?week=2026-03-16", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	expectedMonday := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	if !capturedFrom.Equal(expectedMonday) {
		t.Errorf("expected from=%v, got %v", expectedMonday, capturedFrom)
	}
	expectedTo := expectedMonday.AddDate(0, 0, 7)
	if !capturedTo.Equal(expectedTo) {
		t.Errorf("expected to=%v, got %v", expectedTo, capturedTo)
	}
}

func TestEditorPage_InvalidWeekParam(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return nil, nil
	}
	stubEmptyBuild()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/", ReportEditorPage(cfg, nil))

	req := httptest.NewRequest("GET", "/?week=garbage", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	// Should not fail; should fall back to the current week
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for invalid week param, got %d", rr.Code)
	}
}

func TestReclassifyItem_Success(t *testing.T) {
	restore := saveDeps()
	defer restore()

	var reclassifyCalled bool
	var capturedNewSection string
	web.GetWorkItemByID = func(db *sql.DB, id int64) (web.WorkItem, error) {
		return web.WorkItem{ID: 42, Description: "Test item", Category: "S0_0"}, nil
	}
	web.ReclassifyItem = func(db *sql.DB, c web.ClassificationCorrection, newCategory string) error {
		reclassifyCalled = true
		capturedNewSection = newCategory
		return nil
	}

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Post("/items/{id}/reclassify", ReclassifyItemHandler(cfg, nil))

	body := strings.NewReader("section_id=S1_0")
	req := httptest.NewRequest("POST", "/items/42/reclassify", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !reclassifyCalled {
		t.Error("expected ReclassifyItem to be called")
	}
	if capturedNewSection != "S1_0" {
		t.Errorf("expected new section 'S1_0', got %q", capturedNewSection)
	}
}

func TestReclassifyItem_InvalidSectionID(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetWorkItemByID = func(db *sql.DB, id int64) (web.WorkItem, error) {
		return web.WorkItem{ID: 42}, nil
	}

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Post("/items/{id}/reclassify", ReclassifyItemHandler(cfg, nil))

	body := strings.NewReader("section_id=")
	req := httptest.NewRequest("POST", "/items/42/reclassify", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty section_id, got %d", rr.Code)
	}
}

func TestReclassifyItem_NonManagerForbidden(t *testing.T) {
	restore := saveDeps()
	defer restore()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UDEV1", "dev", false))
	r.Use(mw.RequireManager)
	r.Post("/items/{id}/reclassify", ReclassifyItemHandler(cfg, nil))

	body := strings.NewReader("section_id=S1_0")
	req := httptest.NewRequest("POST", "/items/42/reclassify", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-manager, got %d", rr.Code)
	}
}

func TestUpdateItem_Success(t *testing.T) {
	restore := saveDeps()
	defer restore()

	var capturedID int64
	var capturedDesc, capturedStatus string
	web.UpdateWorkItemTextAndStatus = func(db *sql.DB, id int64, desc, status string) error {
		capturedID = id
		capturedDesc = desc
		capturedStatus = status
		return nil
	}

	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Post("/items/{id}", UpdateItemHandler(nil))

	body := strings.NewReader("description=Updated+description&status=in+progress")
	req := httptest.NewRequest("POST", "/items/99", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if capturedID != 99 {
		t.Errorf("expected item ID 99, got %d", capturedID)
	}
	if capturedDesc != "Updated description" {
		t.Errorf("expected description 'Updated description', got %q", capturedDesc)
	}
	if capturedStatus != "in progress" {
		t.Errorf("expected status 'in progress', got %q", capturedStatus)
	}
}

func TestUpdateItem_EmptyDescription(t *testing.T) {
	restore := saveDeps()
	defer restore()

	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Post("/items/{id}", UpdateItemHandler(nil))

	body := strings.NewReader("description=&status=done")
	req := httptest.NewRequest("POST", "/items/99", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty description, got %d", rr.Code)
	}
}

func TestDeleteItem_Success(t *testing.T) {
	restore := saveDeps()
	defer restore()

	var deletedID int64
	web.DeleteWorkItemByID = func(db *sql.DB, id int64) error {
		deletedID = id
		return nil
	}

	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Delete("/items/{id}", DeleteItemHandler(nil))

	req := httptest.NewRequest("DELETE", "/items/55", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if deletedID != 55 {
		t.Errorf("expected deleted ID 55, got %d", deletedID)
	}
}

func TestDeleteItem_NonManagerForbidden(t *testing.T) {
	restore := saveDeps()
	defer restore()

	r := chi.NewRouter()
	r.Use(authMiddleware("UDEV1", "dev", false))
	r.Use(mw.RequireManager)
	r.Delete("/items/{id}", DeleteItemHandler(nil))

	req := httptest.NewRequest("DELETE", "/items/55", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-manager, got %d", rr.Code)
	}
}

func TestPreviewMarkdown_TeamMode(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return []web.WorkItem{
			{ID: 1, Description: "Test item", Author: "Alice", Source: "slack", Status: "done"},
		}, nil
	}
	web.GetRecentCorrections = func(db *sql.DB, since time.Time, limit int) ([]web.ClassificationCorrection, error) {
		return nil, nil
	}
	web.GetClassifiedItemsWithSections = func(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
		return nil, nil
	}
	web.BuildReportsFromLast = func(cfg web.Config, items []web.WorkItem, reportDate time.Time, corrections []web.ClassificationCorrection, historicalItems []domain.HistoricalItem) (web.BuildResult, error) {
		return web.BuildResult{
			Template: &report.ReportTemplate{},
		}, nil
	}
	var capturedMode string
	web.RenderMarkdownByMode = func(t *web.ReportTemplate, mode string) string {
		capturedMode = mode
		return "# Team Report\n- Test item (done)"
	}
	web.ReportWeekRange = func(cfg web.Config, now time.Time) (time.Time, time.Time) {
		monday := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
		return monday, monday.AddDate(0, 0, 7)
	}

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/preview", PreviewMarkdown(cfg, nil))

	req := httptest.NewRequest("GET", "/preview?mode=team&week=2026-03-16", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if capturedMode != "team" {
		t.Errorf("expected mode 'team', got %q", capturedMode)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Team Report") {
		t.Error("expected rendered markdown to contain 'Team Report'")
	}
}

func TestPreviewMarkdown_BossMode(t *testing.T) {
	restore := saveDeps()
	defer restore()

	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return []web.WorkItem{
			{ID: 1, Description: "Test item", Author: "Alice", Source: "slack", Status: "done"},
		}, nil
	}
	web.GetRecentCorrections = func(db *sql.DB, since time.Time, limit int) ([]web.ClassificationCorrection, error) {
		return nil, nil
	}
	web.GetClassifiedItemsWithSections = func(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
		return nil, nil
	}
	web.BuildReportsFromLast = func(cfg web.Config, items []web.WorkItem, reportDate time.Time, corrections []web.ClassificationCorrection, historicalItems []domain.HistoricalItem) (web.BuildResult, error) {
		return web.BuildResult{
			Template: &report.ReportTemplate{},
		}, nil
	}
	var capturedMode string
	web.RenderMarkdownByMode = func(t *web.ReportTemplate, mode string) string {
		capturedMode = mode
		return "# Boss Report\n- Summary"
	}
	web.ReportWeekRange = func(cfg web.Config, now time.Time) (time.Time, time.Time) {
		monday := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
		return monday, monday.AddDate(0, 0, 7)
	}

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/preview", PreviewMarkdown(cfg, nil))

	req := httptest.NewRequest("GET", "/preview?mode=boss&week=2026-03-16", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if capturedMode != "boss" {
		t.Errorf("expected mode 'boss', got %q", capturedMode)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Boss Report") {
		t.Error("expected rendered markdown to contain 'Boss Report'")
	}
}

func TestGenerateReport_StartsJob(t *testing.T) {
	// NOTE: GenerateReport spawns a background goroutine that uses deps.
	// We must NOT defer restore() before the goroutine completes, or the
	// goroutine will call real deps with nil db and panic.
	// Instead, we keep deps overridden for the lifetime of this test.
	restore := saveDeps()
	defer restore()

	stubEmptyBuild()
	web.GetItemsByDateRange = func(db *sql.DB, from, to time.Time) ([]web.WorkItem, error) {
		return nil, nil
	}
	web.GetRecentCorrections = func(db *sql.DB, since time.Time, limit int) ([]web.ClassificationCorrection, error) {
		return nil, nil
	}
	web.GetClassifiedItemsWithSections = func(db *sql.DB, since time.Time, limit int) ([]domain.HistoricalItem, error) {
		return nil, nil
	}
	web.WriteReportFile = func(content, outputDir string, friday time.Time, teamName string) (string, error) {
		return "/tmp/test_report.md", nil
	}

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Post("/generate", GenerateReport(cfg, nil))

	req := httptest.NewRequest("POST", "/generate?week=2026-03-16", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	// The response should contain a polling div with hx-get for status
	if !strings.Contains(body, "hx-get") {
		t.Error("expected response to contain hx-get polling element")
	}
	if !strings.Contains(body, "generate-status") {
		t.Error("expected response to contain generate-status class")
	}
	if !strings.Contains(body, "/status") {
		t.Error("expected response to contain a /status URL for polling")
	}

	// Wait briefly for background goroutine to complete (it uses stubbed deps, so it's fast)
	time.Sleep(100 * time.Millisecond)
}

func TestGenerateReport_PollStatus(t *testing.T) {
	// Store a job directly in the generateJobs sync.Map
	jobID := "test-job-123"
	job := &generateJob{status: "done", message: "Report generated successfully!", path: "/tmp/report.md"}
	generateJobs.Store(jobID, job)
	defer generateJobs.Delete(jobID)

	r := chi.NewRouter()
	r.Use(authMiddleware("UMGR", "manager", true))
	r.Get("/generate/{jobID}/status", GenerateStatus())

	req := httptest.NewRequest("GET", fmt.Sprintf("/generate/%s/status", jobID), nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Report generated successfully!") {
		t.Errorf("expected success message in body, got: %s", body)
	}
	if !strings.Contains(body, "flash-success") {
		t.Error("expected flash-success class in response")
	}

	// Job should be cleaned up after reading "done" status
	if _, ok := generateJobs.Load(jobID); ok {
		t.Error("expected job to be deleted after returning done status")
	}
}

func TestGenerateReport_NonManagerForbidden(t *testing.T) {
	restore := saveDeps()
	defer restore()

	cfg := testConfig()
	r := chi.NewRouter()
	r.Use(authMiddleware("UDEV1", "dev", false))
	r.Use(mw.RequireManager)
	r.Post("/generate", GenerateReport(cfg, nil))

	req := httptest.NewRequest("POST", "/generate", nil)
	req.AddCookie(sessionCookie())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-manager, got %d", rr.Code)
	}
}
