package handlers

import (
	"database/sql"
	"fmt"
	"html"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/report"
	"reportbot/internal/web"
	"reportbot/internal/web/middleware"
	"reportbot/internal/web/templates"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/csrf"
)

// buildResultCache caches BuildResult per week to avoid re-running the LLM pipeline
// on every HTMX partial swap. Key: Monday date string, Value: cachedResult.
var buildResultCache sync.Map

type cachedResult struct {
	result  web.BuildResult
	created time.Time
}

// generateJobs tracks async report generation jobs.
var generateJobs sync.Map // key: jobID string, value: *generateJob

type generateJob struct {
	mu      sync.Mutex
	status  string // "running", "done", "error"
	message string
	path    string
}

func (j *generateJob) update(status, message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = status
	j.message = message
}

func (j *generateJob) setDone(message, path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = "done"
	j.message = message
	j.path = path
}

func (j *generateJob) read() (status, message, path string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.message, j.path
}

func invalidateCache() {
	buildResultCache = sync.Map{}
}

// ReportEditorPage serves the main editor page.
func ReportEditorPage(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		isManager := middleware.IsManager(r)
		userID := middleware.UserID(r)

		weekParam := r.URL.Query().Get("week")
		monday, weekLabel, prevWeek, nextWeek := resolveWeek(cfg, weekParam)

		from := monday
		to := monday.AddDate(0, 0, 7)

		items, err := web.GetItemsByDateRange(db, from, to)
		if err != nil {
			log.Printf("Error loading items: %v", err)
			http.Error(w, "Failed to load items", http.StatusInternalServerError)
			return
		}

		// Non-managers see only their own items
		if !isManager {
			items = filterByAuthorID(items, userID)
		}

		// Build sections from cached or fresh LLM classification
		sections, avgConf := buildSectionsFromItems(cfg, db, items, monday)

		// Count unique authors
		authorSet := make(map[string]bool)
		for _, item := range items {
			authorSet[strings.TrimSpace(item.Author)] = true
		}

		mode := r.URL.Query().Get("mode")
		if mode != "boss" {
			mode = "team"
		}

		data := templates.EditorData{
			TeamName:    cfg.TeamName,
			WeekLabel:   weekLabel,
			WeekParam:   monday.Format("2006-01-02"),
			PrevWeek:    prevWeek,
			NextWeek:    nextWeek,
			ItemCount:   len(items),
			AuthorCount: len(authorSet),
			AvgConf:     avgConf,
			Sections:    sections,
			IsManager:   isManager,
			CSRFToken:   csrf.Token(r),
			Mode:        mode,
		}

		if err := templates.ReportEditor(data).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering report editor: %v", err)
		}
	}
}

// PreviewMarkdown renders the report as markdown for the preview panel.
// Moved to manager-only routes to prevent non-manager data leak.
func PreviewMarkdown(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		weekParam := r.URL.Query().Get("week")
		monday, _, _, _ := resolveWeek(cfg, weekParam)

		from := monday
		to := monday.AddDate(0, 0, 7)

		items, err := web.GetItemsByDateRange(db, from, to)
		if err != nil {
			log.Printf("Error loading items for preview: %v", err)
			renderPreview(r, w, "Error loading items. Check server logs for details.")
			return
		}

		// Non-managers see only their own items in preview too
		if !middleware.IsManager(r) {
			items = filterByAuthorID(items, middleware.UserID(r))
		}

		corrections, err := web.GetRecentCorrections(db, monday.AddDate(0, -1, 0), 200)
		if err != nil {
			log.Printf("Warning: failed to load corrections for preview (proceeding without): %v", err)
		}
		historicalItems, err := web.GetClassifiedItemsWithSections(db, monday.AddDate(0, -3, 0), 500)
		if err != nil {
			log.Printf("Warning: failed to load historical items for preview (proceeding without): %v", err)
		}

		result, err := web.BuildReportsFromLast(cfg, items, monday, corrections, historicalItems)
		if err != nil {
			log.Printf("Error building report for preview: %v", err)
			renderPreview(r, w, "Error building report. Check server logs for details.")
			return
		}

		mode := r.URL.Query().Get("mode")
		if mode != "boss" {
			mode = "team"
		}

		md := web.RenderMarkdownByMode(result.Template, mode)
		renderPreview(r, w, md)
	}
}

func renderPreview(r *http.Request, w http.ResponseWriter, content string) {
	if err := templates.MarkdownPreview(content).Render(r.Context(), w); err != nil {
		log.Printf("Error rendering markdown preview: %v", err)
	}
}

// ReclassifyItemHandler moves an item to a different section.
func ReclassifyItemHandler(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "Invalid item ID", http.StatusBadRequest)
			return
		}

		newSectionID := r.FormValue("section_id")
		if newSectionID == "" {
			http.Error(w, "Missing section_id", http.StatusBadRequest)
			return
		}

		item, err := web.GetWorkItemByID(db, itemID)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Item not found", http.StatusNotFound)
			} else {
				log.Printf("DB error fetching item %d: %v", itemID, err)
				http.Error(w, "Failed to load item", http.StatusInternalServerError)
			}
			return
		}

		correction := web.ClassificationCorrection{
			WorkItemID:         item.ID,
			OriginalSectionID:  item.Category,
			CorrectedSectionID: newSectionID,
			Description:        item.Description,
			CorrectedBy:        middleware.UserID(r),
		}

		if err := web.ReclassifyItem(db, correction, newSectionID); err != nil {
			log.Printf("Reclassify error: %v", err)
			http.Error(w, "Failed to reclassify", http.StatusInternalServerError)
			return
		}

		invalidateCache()

		// Trigger full page reload via HTMX HX-Redirect header
		w.Header().Set("HX-Redirect", r.Header.Get("HX-Current-URL"))
		w.WriteHeader(http.StatusOK)
	}
}

// UpdateItemHandler updates an item's description and status.
func UpdateItemHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "Invalid item ID", http.StatusBadRequest)
			return
		}

		desc := strings.TrimSpace(r.FormValue("description"))
		status := strings.TrimSpace(r.FormValue("status"))

		if desc == "" {
			http.Error(w, "Description cannot be empty", http.StatusBadRequest)
			return
		}

		if err := web.UpdateWorkItemTextAndStatus(db, itemID, desc, status); err != nil {
			log.Printf("Update item error: %v", err)
			http.Error(w, "Failed to update item", http.StatusInternalServerError)
			return
		}

		invalidateCache()
		w.Header().Set("HX-Redirect", r.Header.Get("HX-Current-URL"))
		w.WriteHeader(http.StatusOK)
	}
}

// DeleteItemHandler deletes an item.
func DeleteItemHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "Invalid item ID", http.StatusBadRequest)
			return
		}

		if err := web.DeleteWorkItemByID(db, itemID); err != nil {
			log.Printf("Delete item error: %v", err)
			http.Error(w, "Failed to delete item", http.StatusInternalServerError)
			return
		}

		invalidateCache()
		// Return 200 with empty body; HTMX will remove the element via hx-swap="outerHTML"
		w.WriteHeader(http.StatusOK)
	}
}

// EditItemForm returns an inline edit form for an item.
func EditItemForm(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		itemID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "Invalid item ID", http.StatusBadRequest)
			return
		}

		item, err := web.GetWorkItemByID(db, itemID)
		if err != nil {
			if err == sql.ErrNoRows {
				http.Error(w, "Item not found", http.StatusNotFound)
			} else {
				log.Printf("DB error fetching item %d for edit: %v", itemID, err)
				http.Error(w, "Failed to load item", http.StatusInternalServerError)
			}
			return
		}

		if err := templates.ItemEditForm(item.ID, item.Description, item.Status).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering edit form: %v", err)
		}
	}
}

// GenerateReport starts async report generation.
func GenerateReport(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract params before spawning goroutine (don't capture *http.Request in closure)
		weekParam := r.URL.Query().Get("week")
		monday, _, _, _ := resolveWeek(cfg, weekParam)

		jobID := fmt.Sprintf("gen-%d", rand.Int63())
		job := &generateJob{status: "running", message: "Starting report generation..."}
		generateJobs.Store(jobID, job)

		go func() {
			report.GenerationMu.Lock()
			defer report.GenerationMu.Unlock()

			from := monday
			to := monday.AddDate(0, 0, 7)

			job.update("running", "Loading items...")
			items, err := web.GetItemsByDateRange(db, from, to)
			if err != nil {
				job.update("error", "Failed to load items. Check server logs.")
				log.Printf("Generate: failed to load items: %v", err)
				return
			}

			job.update("running", "Classifying items...")
			corrections, err := web.GetRecentCorrections(db, monday.AddDate(0, -1, 0), 200)
			if err != nil {
				log.Printf("Generate: warning: failed to load corrections (proceeding without): %v", err)
			}
			historicalItems, err := web.GetClassifiedItemsWithSections(db, monday.AddDate(0, -3, 0), 500)
			if err != nil {
				log.Printf("Generate: warning: failed to load historical items (proceeding without): %v", err)
			}

			result, err := web.BuildReportsFromLast(cfg, items, monday, corrections, historicalItems)
			if err != nil {
				job.update("error", "Classification failed. Check server logs.")
				log.Printf("Generate: classification failed: %v", err)
				return
			}

			job.update("running", "Writing report files...")
			md := web.RenderMarkdownByMode(result.Template, "team")
			friday := domain.FridayOfWeek(monday)
			path, err := web.WriteReportFile(md, cfg.ReportOutputDir, friday, cfg.TeamName)
			if err != nil {
				job.update("error", "Failed to write report. Check server logs.")
				log.Printf("Generate: failed to write report: %v", err)
				return
			}

			invalidateCache()
			job.setDone("Report generated successfully!", path)
		}()

		// Return polling element with HTML-escaped jobID
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		safeID := html.EscapeString(jobID)
		fmt.Fprintf(w, `<div class="generate-status" hx-get="/generate/%s/status" hx-trigger="every 2s" hx-swap="innerHTML"><div class="spinner"></div><span>Starting report generation...</span></div>`, safeID)
	}
}

// GenerateStatus returns the current status of a generation job.
func GenerateStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID := chi.URLParam(r, "jobID")
		val, ok := generateJobs.Load(jobID)
		if !ok {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}

		job := val.(*generateJob)
		status, message, _ := job.read()
		safeID := html.EscapeString(jobID)
		safeMsg := html.EscapeString(message)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		switch status {
		case "running":
			fmt.Fprintf(w, `<div class="generate-status" hx-get="/generate/%s/status" hx-trigger="every 2s" hx-swap="innerHTML"><div class="spinner"></div><span>%s</span></div>`, safeID, safeMsg)
		case "done":
			generateJobs.Delete(jobID)
			fmt.Fprintf(w, `<div class="flash flash-success">%s</div>`, safeMsg)
		case "error":
			generateJobs.Delete(jobID)
			fmt.Fprintf(w, `<div class="flash flash-error">%s</div>`, safeMsg)
		}
	}
}

// --- Helpers ---

func resolveWeek(cfg web.Config, weekParam string) (monday time.Time, label, prevWeek, nextWeek string) {
	now := time.Now()
	if cfg.Location != nil {
		now = now.In(cfg.Location)
	}

	if weekParam != "" {
		if parsed, err := time.Parse("2006-01-02", weekParam); err == nil {
			// Round to Monday
			for parsed.Weekday() != time.Monday {
				parsed = parsed.AddDate(0, 0, -1)
			}
			monday = parsed
		}
	}

	if monday.IsZero() {
		// Use ReportWeekRange to match Slack behavior (including monday_cutoff_time)
		start, _ := web.ReportWeekRange(cfg, now)
		monday = start
	}

	sunday := monday.AddDate(0, 0, 6)
	label = fmt.Sprintf("%s - %s, %d", monday.Format("Jan 2"), sunday.Format("Jan 2"), monday.Year())
	prevWeek = monday.AddDate(0, 0, -7).Format("2006-01-02")
	nextWeek = monday.AddDate(0, 0, 7).Format("2006-01-02")
	return
}

func buildSectionsFromItems(cfg web.Config, db *sql.DB, items []web.WorkItem, monday time.Time) ([]templates.SectionData, float64) {
	if len(items) == 0 {
		return nil, 0
	}

	cacheKey := monday.Format("2006-01-02")

	// Check cache first
	if cached, ok := buildResultCache.Load(cacheKey); ok {
		cr := cached.(*cachedResult)
		// Cache valid for 5 minutes
		if time.Since(cr.created) < 5*time.Minute {
			return convertBuildResult(cr.result, items), avgConfidence(cr.result.Decisions)
		}
		buildResultCache.Delete(cacheKey)
	}

	corrections, err := web.GetRecentCorrections(db, monday.AddDate(0, -1, 0), 200)
	if err != nil {
		log.Printf("Warning: failed to load corrections (proceeding without): %v", err)
	}
	historicalItems, err := web.GetClassifiedItemsWithSections(db, monday.AddDate(0, -3, 0), 500)
	if err != nil {
		log.Printf("Warning: failed to load historical items (proceeding without): %v", err)
	}

	result, err := web.BuildReportsFromLast(cfg, items, monday, corrections, historicalItems)
	if err != nil {
		log.Printf("BuildReportsFromLast failed (showing unclassified): %v", err)
		return buildUnclassifiedSection(items), 0
	}

	// Store in cache
	buildResultCache.Store(cacheKey, &cachedResult{result: result, created: time.Now()})

	return convertBuildResult(result, items), avgConfidence(result.Decisions)
}

func buildUnclassifiedSection(items []web.WorkItem) []templates.SectionData {
	var itemData []templates.ItemData
	for _, item := range items {
		itemData = append(itemData, toItemData(item, "UND", 0))
	}
	return []templates.SectionData{{
		ID:          "UND",
		Name:        "Unclassified",
		Items:       itemData,
		NeedsReview: true,
	}}
}

func convertBuildResult(result web.BuildResult, items []web.WorkItem) []templates.SectionData {
	if result.Template == nil {
		return nil
	}

	// Build item lookup
	itemMap := make(map[string]web.WorkItem)
	for _, item := range items {
		itemMap[strings.ToLower(strings.TrimSpace(item.Description))] = item
	}

	var sections []templates.SectionData
	for ci, cat := range result.Template.Categories {
		if cat.MarkerLine != "" {
			continue
		}
		sectionID := fmt.Sprintf("S%d", ci)
		if len(result.Options) > ci {
			sectionID = result.Options[ci].ID
		}

		var sectionItems []templates.ItemData
		needsReview := false

		for _, sub := range cat.Subsections {
			for _, tItem := range sub.Items {
				if !tItem.IsNew {
					continue // Skip carry-over items from template
				}
				// Find the original item to get metadata
				key := strings.ToLower(strings.TrimSpace(tItem.Description))
				origItem, found := itemMap[key]

				conf := 0.0
				source := "slack"
				sourceRef := ""
				var id int64

				if found {
					id = origItem.ID
					source = origItem.Source
					sourceRef = origItem.SourceRef
					if d, ok := result.Decisions[origItem.ID]; ok {
						conf = d.Confidence
					}
				} else {
					// Item not found in map — skip rendering mutation controls
					// by leaving ID as 0 (template checks for this)
					log.Printf("Warning: template item %q not found in item map, skipping mutation controls", tItem.Description)
				}

				if conf < 0.7 {
					needsReview = true
				}

				sectionItems = append(sectionItems, templates.ItemData{
					ID:          id,
					Description: tItem.Description,
					Author:      tItem.Author,
					Status:      tItem.Status,
					Source:      source,
					SourceRef:   sourceRef,
					Confidence:  conf,
					SectionID:   sectionID,
					TicketIDs:   tItem.TicketIDs,
					IsNew:       tItem.IsNew,
				})
			}
		}

		if len(sectionItems) == 0 {
			continue
		}

		sections = append(sections, templates.SectionData{
			ID:          sectionID,
			Name:        cat.Name,
			Items:       sectionItems,
			NeedsReview: needsReview,
		})
	}

	return sections
}

func toItemData(item web.WorkItem, sectionID string, conf float64) templates.ItemData {
	return templates.ItemData{
		ID:          item.ID,
		Description: item.Description,
		Author:      item.Author,
		Status:      item.Status,
		Source:      item.Source,
		SourceRef:   item.SourceRef,
		Confidence:  conf,
		SectionID:   sectionID,
		TicketIDs:   item.TicketIDs,
	}
}

func avgConfidence(decisions map[int64]web.LLMSectionDecision) float64 {
	if len(decisions) == 0 {
		return 0
	}
	var total float64
	for _, d := range decisions {
		total += d.Confidence
	}
	return total / float64(len(decisions))
}

func filterByAuthorID(items []web.WorkItem, userID string) []web.WorkItem {
	var filtered []web.WorkItem
	for _, item := range items {
		if item.AuthorID == userID {
			filtered = append(filtered, item)
		}
	}
	return filtered
}
