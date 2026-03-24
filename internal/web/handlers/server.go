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
	"github.com/gorilla/csrf"

	"reportbot/internal/web"
	mw "reportbot/internal/web/middleware"
)

// NewServer creates and configures the HTTP server with chi router.
func NewServer(cfg web.Config, db *sql.DB) *http.Server {
	r := chi.NewRouter()

	// Built-in chi middleware
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)

	// Derive Secure flag from base URL scheme
	isHTTPS := strings.HasPrefix(cfg.WebBaseURL, "https://")

	// CSRF protection
	csrfMiddleware := csrf.Protect(
		[]byte(cfg.WebSessionSecret),
		csrf.Secure(isHTTPS),
		csrf.Path("/"),
		csrf.RequestHeader("X-CSRF-Token"),
	)
	r.Use(csrfMiddleware)

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

		// Report editor (read-only for non-managers, preview filters by author)
		r.Get("/", ReportEditorPage(cfg, db))
		r.Get("/preview", PreviewMarkdown(cfg, db))

		// Manager-only mutation routes
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireManager)

			r.Post("/items/{id}/reclassify", ReclassifyItemHandler(cfg, db))
			r.Post("/items/{id}", UpdateItemHandler(db))
			r.Post("/items/{id}/delete", DeleteItemHandler(db))
			r.Get("/items/{id}/edit", EditItemForm(db))
			r.Get("/items/{id}/view", ViewItemRow(db))
			r.Post("/generate", GenerateReport(cfg, db))
			r.Get("/generate/{jobID}/status", GenerateStatus())
		})
	})

	return &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.WebPort),
		Handler: r,
	}
}
