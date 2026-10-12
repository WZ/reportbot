package handlers

import (
	"database/sql"
	"log"
	"net/http"

	"reportbot/internal/web"
	"reportbot/internal/web/middleware"
	"reportbot/internal/web/templates"

	"github.com/go-chi/chi/v5"
)

func PublishedList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		weeks, err := web.ListPublishedWeeks(db)
		if err != nil {
			log.Printf("Error listing published reports: %v", err)
			http.Error(w, "Failed to list Published Reports", http.StatusInternalServerError)
			return
		}
		if err := templates.PublishedList(web.GetCSRFToken(r), middleware.IsManager(r), weeks).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering published list: %v", err)
		}
	}
}

// PublishedReport shows one week's Published Report, team version unless ?mode=boss.
func PublishedReport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		week := chi.URLParam(r, "week")
		rep, ok, err := web.GetPublishedReport(db, week)
		if err != nil {
			log.Printf("Error loading published report %s: %v", week, err)
			http.Error(w, "Failed to load the Published Report", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "No Published Report for that week", http.StatusNotFound)
			return
		}
		mode, md := "team", rep.TeamMarkdown
		if r.URL.Query().Get("mode") == "boss" {
			mode, md = "boss", rep.BossMarkdown
		}
		data := templates.PublishedReportData{
			CSRFToken:   web.GetCSRFToken(r),
			IsManager:   middleware.IsManager(r),
			Week:        rep.WeekStart,
			Mode:        mode,
			PublishedBy: rep.PublishedBy,
			PublishedAt: rep.PublishedAt.Format("2006-01-02 15:04"),
			HTML:        simpleMarkdownToHTML(md),
		}
		if err := templates.PublishedReport(data).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering published report: %v", err)
		}
	}
}
