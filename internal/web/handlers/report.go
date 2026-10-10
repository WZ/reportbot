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
	"reportbot/internal/filing"
	"reportbot/internal/report"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web"
	"reportbot/internal/web/middleware"
	"reportbot/internal/web/templates"

	"github.com/go-chi/chi/v5"
)

// generateJobs tracks async publish jobs.
var generateJobs sync.Map // key: jobID string, value: *generateJob

type generateJob struct {
	mu      sync.Mutex
	status  string // "running", "done", "error"
	message string
}

func (j *generateJob) update(status, message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status = status
	j.message = message
}

func (j *generateJob) read() (status, message string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status, j.message
}

// ReportPage serves the live Weekly Report.
func ReportPage(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		monday, label, prevWeek, nextWeek := resolveWeek(cfg, r.URL.Query().Get("week"))
		body, err := loadReportBody(cfg, db, r, monday)
		if err != nil {
			log.Printf("Error loading live report: %v", err)
			http.Error(w, "Failed to load the report", http.StatusInternalServerError)
			return
		}
		data := templates.ReportPageData{
			WeekLabel: label,
			WeekParam: monday.Format("2006-01-02"),
			PrevWeek:  prevWeek,
			NextWeek:  nextWeek,
			IsManager: middleware.IsManager(r),
			CSRFToken: web.GetCSRFToken(r),
			Body:      body,
		}
		if err := templates.ReportPage(data).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering live report: %v", err)
		}
	}
}

// ReportBody returns the report body partial, or 204 when nothing changed since version v.
func ReportBody(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version, err := web.ReportVersion(db)
		if err != nil {
			log.Printf("Error reading report version: %v", err)
			http.Error(w, "Failed to load the report", http.StatusInternalServerError)
			return
		}
		if version == r.URL.Query().Get("v") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		monday, _, _, _ := resolveWeek(cfg, r.URL.Query().Get("week"))
		body, err := loadReportBody(cfg, db, r, monday)
		if err != nil {
			log.Printf("Error loading live report: %v", err)
			http.Error(w, "Failed to load the report", http.StatusInternalServerError)
			return
		}
		if err := templates.ReportBody(body).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering report body: %v", err)
		}
	}
}

// loadReportBody reads the version first so a change made while loading shows on the next poll.
func loadReportBody(cfg web.Config, db *sql.DB, r *http.Request, monday time.Time) (templates.ReportBodyData, error) {
	version, err := web.ReportVersion(db)
	if err != nil {
		return templates.ReportBodyData{}, err
	}
	live, err := web.LoadRange(db, monday, monday.AddDate(0, 0, 7))
	if err != nil {
		return templates.ReportBodyData{}, err
	}
	groups := sectionGroups(live.Taxonomy)
	toItems := func(items []web.WorkItem) []templates.ItemData {
		out := make([]templates.ItemData, 0, len(items))
		for _, it := range items {
			out = append(out, itemData(cfg, it, canEdit(r, it), groups))
		}
		return out
	}
	body := templates.ReportBodyData{
		WeekParam:    monday.Format("2006-01-02"),
		Version:      version,
		ItemCount:    live.View.ItemCount(),
		Undetermined: toItems(live.View.Undetermined),
	}
	for _, a := range live.View.Areas {
		area := templates.AreaData{Name: a.Name}
		for _, c := range a.Categories {
			cat := templates.CategoryData{Name: c.Name, Path: a.Name + domain.PathSeparator + c.Name, Items: toItems(c.Items)}
			for _, s := range c.Sections {
				cat.Sections = append(cat.Sections, templates.SectionData{Name: s.Section.Label(), Path: s.Section.Path(), Items: toItems(s.Items)})
			}
			area.Categories = append(area.Categories, cat)
		}
		body.Areas = append(body.Areas, area)
	}
	return body, nil
}

// PreviewMarkdown renders the live view as team or boss markdown.
func PreviewMarkdown(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		monday, _, _, _ := resolveWeek(cfg, r.URL.Query().Get("week"))
		live, err := web.LoadRange(db, monday, monday.AddDate(0, 0, 7))
		if err != nil {
			log.Printf("Error loading report for preview: %v", err)
			if err := templates.MarkdownPreviewRaw("Error loading the report. Check server logs for details.").Render(r.Context(), w); err != nil {
				log.Printf("Error rendering preview error: %v", err)
			}
			return
		}
		md := report.RenderMarkdownByMode(live.View, r.URL.Query().Get("mode"))
		if err := templates.MarkdownPreviewHTML(simpleMarkdownToHTML(md)).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering markdown preview: %v", err)
		}
	}
}

// canEdit allows the Work Item's owner and managers.
func canEdit(r *http.Request, item web.WorkItem) bool {
	if middleware.IsManager(r) {
		return true
	}
	// Only the Slack user ID counts: display names are user-editable. Items without one are manager-only.
	return item.AuthorID != "" && item.AuthorID == middleware.UserID(r)
}

// loadEditableItem resolves {id} and writes the error response when the viewer may not edit it.
func loadEditableItem(w http.ResponseWriter, r *http.Request, db *sql.DB) (web.WorkItem, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid item ID", http.StatusBadRequest)
		return web.WorkItem{}, false
	}
	item, err := web.GetWorkItemByID(db, id)
	if err == sql.ErrNoRows {
		http.Error(w, "Work Item not found", http.StatusNotFound)
		return item, false
	}
	if err != nil {
		log.Printf("Error loading item %d: %v", id, err)
		http.Error(w, "Failed to load Work Item", http.StatusInternalServerError)
		return item, false
	}
	if !canEdit(r, item) {
		http.Error(w, "Forbidden: only the author or a manager can change this Work Item", http.StatusForbidden)
		return item, false
	}
	return item, true
}

// renderItem renders the item's row, or its edit form when edit is set.
func renderItem(w http.ResponseWriter, r *http.Request, cfg web.Config, db *sql.DB, id int64, edit bool) {
	item, err := web.GetWorkItemByID(db, id)
	if err != nil {
		log.Printf("Error reloading item %d: %v", id, err)
		http.Error(w, "Failed to load Work Item", http.StatusInternalServerError)
		return
	}
	tax, err := web.LoadTaxonomy(db)
	if err != nil {
		log.Printf("Error loading taxonomy: %v", err)
		http.Error(w, "Failed to load Taxonomy", http.StatusInternalServerError)
		return
	}
	data := itemData(cfg, item, true, sectionGroups(tax))
	if edit {
		err = templates.ItemEditForm(data, domain.Statuses).Render(r.Context(), w)
	} else {
		err = templates.ItemRow(data).Render(r.Context(), w)
	}
	if err != nil {
		log.Printf("Error rendering item %d: %v", id, err)
	}
}

// reportChanged makes the page refresh its report body after the response is swapped in.
func reportChanged(w http.ResponseWriter) {
	w.Header().Set("HX-Trigger-After-Settle", "report-changed")
}

func EditItemForm(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if item, ok := loadEditableItem(w, r, db); ok {
			renderItem(w, r, cfg, db, item.ID, true)
		}
	}
}

func ViewItemRow(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if item, ok := loadEditableItem(w, r, db); ok {
			renderItem(w, r, cfg, db, item.ID, false)
		}
	}
}

// UpdateItem saves text, Status, Status Note and tickets. Picking another Section is a
// person's Placement; otherwise changed text sends the Work Item back for filing.
func UpdateItem(cfg web.Config, db *sql.DB, wake func()) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok := loadEditableItem(w, r, db)
		if !ok {
			return
		}
		desc := strings.TrimSpace(r.FormValue("description"))
		status := r.FormValue("status")
		if desc == "" {
			http.Error(w, "Description is required", http.StatusBadRequest)
			return
		}
		if !domain.IsStatus(status) {
			http.Error(w, "Invalid status", http.StatusBadRequest)
			return
		}
		sectionID := item.SectionID
		if v := r.FormValue("section_id"); v != "" {
			id, err := parseSectionID(db, v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			sectionID = id
		}
		if err := web.UpdateWorkItemFields(db, item.ID, desc, status, strings.TrimSpace(r.FormValue("status_note")), strings.TrimSpace(r.FormValue("ticket_ids"))); err != nil {
			log.Printf("Error updating item %d: %v", item.ID, err)
			http.Error(w, "Failed to save Work Item", http.StatusInternalServerError)
			return
		}
		switch {
		case sectionID != item.SectionID:
			if err := placeByPerson(db, item.ID, sectionID, middleware.UserID(r)); err != nil {
				log.Printf("Error placing item %d: %v", item.ID, err)
				http.Error(w, "Failed to move Work Item", http.StatusInternalServerError)
				return
			}
		case desc != item.Description:
			if err := web.SetFilingPending(db, item.ID, filing.Payload{Kind: "edit"}.JSON()); err != nil {
				log.Printf("Error re-queuing filing for item %d: %v", item.ID, err)
				http.Error(w, "Saved, but failed to re-file the Work Item", http.StatusInternalServerError)
				return
			}
			wake()
		}
		reportChanged(w)
		renderItem(w, r, cfg, db, item.ID, false)
	}
}

// MoveItem files the Work Item in the chosen Section (0 = Undetermined) as a person's decision.
func MoveItem(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok := loadEditableItem(w, r, db)
		if !ok {
			return
		}
		sectionID, err := parseSectionID(db, r.FormValue("section_id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := placeByPerson(db, item.ID, sectionID, middleware.UserID(r)); err != nil {
			log.Printf("Error moving item %d: %v", item.ID, err)
			http.Error(w, "Failed to move Work Item", http.StatusInternalServerError)
			return
		}
		reportChanged(w)
		renderItem(w, r, cfg, db, item.ID, false)
	}
}

func DeleteItem(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, ok := loadEditableItem(w, r, db)
		if !ok {
			return
		}
		if err := web.DeleteWorkItemByID(db, item.ID); err != nil {
			log.Printf("Error deleting item %d: %v", item.ID, err)
			http.Error(w, "Failed to delete Work Item", http.StatusInternalServerError)
			return
		}
		reportChanged(w)
		w.WriteHeader(http.StatusOK)
	}
}

// parseSectionID accepts 0 (Undetermined) or an active Section.
func parseSectionID(db *sql.DB, v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid section_id")
	}
	if id == 0 {
		return 0, nil
	}
	tax, err := web.LoadTaxonomy(db)
	if err != nil {
		return 0, err
	}
	if _, ok := tax.Section(id); !ok {
		return 0, fmt.Errorf("unknown Section")
	}
	return id, nil
}

func placeByPerson(db *sql.DB, itemID, sectionID int64, actorID string) error {
	return web.SetPlacement(db, itemID, sqlite.Placement{
		SectionID:   sectionID,
		PlacedBy:    domain.PlacedByPerson,
		HistoryNote: sqlite.ReasonWebMove,
		ActorID:     actorID,
	})
}

// GenerateReport publishes the week. With Undetermined Work Items it first asks for confirmation.
func GenerateReport(cfg web.Config, db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		monday, _, _, _ := resolveWeek(cfg, r.URL.Query().Get("week"))
		week := monday.Format("2006-01-02")
		userID := middleware.UserID(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		live, err := web.LoadRange(db, monday, monday.AddDate(0, 0, 7))
		if err != nil {
			log.Printf("Generate: failed to load report: %v", err)
			fmt.Fprint(w, `<div class="flash flash-error">Failed to load the report. Check server logs.</div>`)
			return
		}
		if live.View.ItemCount() == 0 {
			fmt.Fprint(w, `<div class="flash flash-info">No Work Items this week.</div>`)
			return
		}
		if n := len(live.View.Undetermined); n > 0 && r.FormValue("confirm") != "1" {
			fmt.Fprintf(w, `<div class="flash flash-warning">%d Work Item(s) are still in Undetermined. `+
				`<button class="btn btn-secondary" hx-post="/generate?week=%s&amp;confirm=1" hx-target="#generate-status" hx-swap="innerHTML">Publish anyway</button></div>`,
				n, html.EscapeString(week))
			return
		}

		jobID := fmt.Sprintf("gen-%d", rand.Int63())
		job := &generateJob{status: "running", message: "Publishing..."}
		generateJobs.Store(jobID, job)

		go func() {
			report.GenerationMu.Lock()
			defer report.GenerationMu.Unlock()

			live, err := web.LoadRange(db, monday, monday.AddDate(0, 0, 7))
			if err != nil {
				job.update("error", "Failed to load the report. Check server logs.")
				log.Printf("Generate: failed to load report: %v", err)
				return
			}
			res, err := web.Publish(db, cfg, live, userID)
			if err != nil {
				job.update("error", "Failed to publish. Check server logs.")
				log.Printf("Generate: publish failed: %v", err)
				return
			}
			job.update("done", fmt.Sprintf("Published %d Work Items. Saved to %s", live.View.ItemCount(), res.TeamPath))
		}()

		fmt.Fprintf(w, `<div class="generate-status" hx-get="/generate/%s/status" hx-trigger="every 2s" hx-swap="outerHTML"><div class="spinner"></div><span>Publishing...</span></div>`,
			html.EscapeString(jobID))
	}
}

// GenerateStatus returns the current status of a publish job.
func GenerateStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobID := chi.URLParam(r, "jobID")
		val, ok := generateJobs.Load(jobID)
		if !ok {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}
		status, message := val.(*generateJob).read()
		safeMsg := html.EscapeString(message)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch status {
		case "running":
			fmt.Fprintf(w, `<div class="generate-status" hx-get="/generate/%s/status" hx-trigger="every 2s" hx-swap="outerHTML"><div class="spinner"></div><span>%s</span></div>`,
				html.EscapeString(jobID), safeMsg)
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

func itemData(cfg web.Config, it web.WorkItem, editable bool, groups []templates.OptionGroup) templates.ItemData {
	d := templates.ItemData{
		ID:                 it.ID,
		Author:             it.Author,
		Description:        it.Description,
		Status:             it.Status,
		StatusNote:         it.StatusNote,
		StatusText:         report.StatusText(it),
		TicketIDs:          it.TicketIDs,
		Source:             it.Source,
		SectionID:          it.SectionID,
		PlacedBy:           it.PlacedBy,
		UndeterminedReason: it.UndeterminedReason,
		CanEdit:            editable,
	}
	if editable {
		d.SectionGroups = groups
	}
	if it.Confidence != nil {
		d.Confidence = fmt.Sprintf("%.0f%%", *it.Confidence*100)
	}
	switch {
	case it.FilingState == domain.FilingPending:
		d.Attention = "filing…"
	case it.FilingState == domain.FilingFailed:
		d.Attention = "filing failed"
	case it.FilingState == domain.FilingAsking:
		d.Attention = "asking the reporter"
	case it.PlacedBy == domain.PlacedByModel && it.Confidence != nil && *it.Confidence < cfg.DecisionModelConfidence:
		d.Attention = "low confidence"
	}
	return d
}

// sectionGroups lists the active Sections as "Area › Category" groups in Taxonomy order.
func sectionGroups(tax domain.Taxonomy) []templates.OptionGroup {
	var groups []templates.OptionGroup
	for _, s := range tax.Sections() {
		label := s.Area + domain.PathSeparator + s.Category
		if len(groups) == 0 || groups[len(groups)-1].Label != label {
			groups = append(groups, templates.OptionGroup{Label: label})
		}
		g := &groups[len(groups)-1]
		g.Options = append(g.Options, templates.Option{ID: s.ID, Name: s.Label()})
	}
	return groups
}

func resolveWeek(cfg web.Config, weekParam string) (monday time.Time, label, prevWeek, nextWeek string) {
	loc := cfg.Location
	if loc == nil {
		loc = time.Local
	}
	if parsed, err := time.ParseInLocation("2006-01-02", weekParam, loc); err == nil {
		monday, _ = domain.CurrentWeekRangeAt(parsed)
	} else {
		monday, _ = web.ReportWeekRange(cfg, time.Now().In(loc))
	}
	sunday := monday.AddDate(0, 0, 6)
	label = fmt.Sprintf("%s - %s, %d", monday.Format("Jan 2"), sunday.Format("Jan 2"), monday.Year())
	prevWeek = monday.AddDate(0, 0, -7).Format("2006-01-02")
	nextWeek = monday.AddDate(0, 0, 7).Format("2006-01-02")
	return
}

// simpleMarkdownToHTML converts report markdown to readable HTML.
// Handles the heading/bullet/bold patterns used by ReportBot reports.
func simpleMarkdownToHTML(md string) string {
	var buf strings.Builder
	lines := strings.Split(md, "\n")
	inList := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Close list if needed
		if inList && !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "  - ") {
			buf.WriteString("</ul>\n")
			inList = false
		}

		switch {
		case trimmed == "":
			if !inList {
				buf.WriteString("<br>\n")
			}
		case strings.HasPrefix(trimmed, "#### "):
			buf.WriteString("<h4>" + html.EscapeString(strings.TrimPrefix(trimmed, "#### ")) + "</h4>\n")
		case strings.HasPrefix(trimmed, "### "):
			buf.WriteString("<h3>" + html.EscapeString(strings.TrimPrefix(trimmed, "### ")) + "</h3>\n")
		case strings.HasPrefix(trimmed, "## "):
			buf.WriteString("<h2>" + html.EscapeString(strings.TrimPrefix(trimmed, "## ")) + "</h2>\n")
		case strings.HasPrefix(trimmed, "# "):
			buf.WriteString("<h1>" + html.EscapeString(strings.TrimPrefix(trimmed, "# ")) + "</h1>\n")
		case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "  - "):
			if !inList {
				buf.WriteString("<ul>\n")
				inList = true
			}
			text := strings.TrimPrefix(strings.TrimPrefix(trimmed, "  "), "- ")
			buf.WriteString("<li>" + inlineBold(html.EscapeString(text)) + "</li>\n")
		default:
			buf.WriteString("<p>" + inlineBold(html.EscapeString(trimmed)) + "</p>\n")
		}
	}

	if inList {
		buf.WriteString("</ul>\n")
	}

	return buf.String()
}

// inlineBold converts **text** to <strong>text</strong> in already-escaped HTML.
func inlineBold(s string) string {
	result := s
	for {
		start := strings.Index(result, "**")
		if start == -1 {
			break
		}
		end := strings.Index(result[start+2:], "**")
		if end == -1 {
			break
		}
		end += start + 2
		inner := result[start+2 : end]
		result = result[:start] + "<strong>" + inner + "</strong>" + result[end+2:]
	}
	return result
}
