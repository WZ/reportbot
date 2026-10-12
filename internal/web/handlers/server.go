package handlers

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"reportbot/internal/web"
	mw "reportbot/internal/web/middleware"
)

// NewServer creates and configures the HTTP server with chi router.
// wake nudges the filing worker after a Work Item or the Taxonomy changes.
func NewServer(cfg web.Config, db *sql.DB, wake func(), hooks web.PublishHooks) *http.Server {
	r := chi.NewRouter()

	// Built-in chi middleware
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)

	// Derive Secure flag from base URL scheme
	_ = strings.HasPrefix(cfg.WebBaseURL, "https://")

	// CSRF protection — double-submit cookie (replaces gorilla/csrf)
	r.Use(web.CSRFMiddleware)

	// Static files (embedded in web package)
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to load embedded static files: %v", err)
	}
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Auth middleware adapter — pass functions to break middleware <-> web import cycle
	authMiddleware := mw.Auth(
		func(cookie *http.Cookie) (mw.SessionPayload, error) {
			payload, err := web.ValidateSessionCookie(cfg.WebSessionSecret, cookie)
			if err != nil {
				return mw.SessionPayload{}, err
			}
			return mw.SessionPayload{UserID: payload.UserID, UserName: payload.UserName}, nil
		},
		func() *http.Cookie { return web.ClearSessionCookie() },
		func(userID string) bool { return web.IsManagerID(cfg, userID) },
	)

	// Public routes
	r.Group(func(r chi.Router) {
		r.Get("/login", LoginPage(cfg))
		r.Get("/auth/slack/callback", SlackOAuthCallback(cfg))
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(authMiddleware)

		r.Post("/logout", Logout())

		r.Get("/", ReportPage(cfg, db))
		r.Get("/report/body", ReportBody(cfg, db))
		r.Get("/preview", PreviewMarkdown(cfg, db))
		r.Get("/published", PublishedList(db))
		r.Get("/published/{week}", PublishedReport(db))

		// Owner or manager; checked per Work Item
		r.Get("/items/{id}/edit", EditItemForm(cfg, db))
		r.Get("/items/{id}/view", ViewItemRow(cfg, db))
		r.Post("/items/{id}", UpdateItem(cfg, db, wake))
		r.Post("/items/{id}/move", MoveItem(cfg, db))
		r.Post("/items/{id}/delete", DeleteItem(db))

		r.Group(func(r chi.Router) {
			r.Use(mw.RequireManager)

			r.Post("/generate", GenerateReport(cfg, db, hooks))
			r.Get("/generate/{jobID}/status", GenerateStatus())
			r.Get("/taxonomy", TaxonomyPage(db))
			r.Post("/taxonomy/nodes", AddTaxonomyNode(db, wake))
			r.Post("/taxonomy/nodes/{id}", UpdateTaxonomyNode(db, wake))
			r.Post("/taxonomy/nodes/{id}/move", MoveTaxonomyNode(db, wake))
			r.Post("/taxonomy/nodes/{id}/retire", RetireTaxonomyNode(db, wake))
			r.Post("/taxonomy/nodes/{id}/merge", MergeTaxonomySection(db, wake))
			r.Post("/taxonomy/nodes/{id}/split", SplitTaxonomySection(db, wake))
		})
	})

	return &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.WebPort),
		Handler: r,
	}
}
