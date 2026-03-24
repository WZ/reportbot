package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

const csrfCookieName = "_csrf"
const csrfHeaderName = "X-CSRF-Token"
const csrfTokenLength = 32

// CSRFMiddleware implements double-submit cookie CSRF protection.
// On GET requests: sets a CSRF cookie if not present.
// On POST/PUT/DELETE requests: validates that the X-CSRF-Token header matches the cookie.
func CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET", "HEAD", "OPTIONS":
			// Safe methods — ensure CSRF cookie exists
			if _, err := r.Cookie(csrfCookieName); err != nil {
				token := generateCSRFToken()
				http.SetCookie(w, &http.Cookie{
					Name:     csrfCookieName,
					Value:    token,
					Path:     "/",
					HttpOnly: false, // Must be readable by JavaScript
					SameSite: http.SameSiteLaxMode,
					MaxAge:   86400,
				})
			}
			next.ServeHTTP(w, r)

		default:
			// Mutation methods — validate token
			cookie, err := r.Cookie(csrfCookieName)
			if err != nil || cookie.Value == "" {
				http.Error(w, "CSRF cookie missing", http.StatusForbidden)
				return
			}
			headerToken := r.Header.Get(csrfHeaderName)
			if headerToken == "" {
				headerToken = r.FormValue("csrf_token")
			}
			if headerToken != cookie.Value {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}
	})
}

// GetCSRFToken extracts the CSRF token from the request cookie.
// Used by handlers to pass the token to templates.
func GetCSRFToken(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func generateCSRFToken() string {
	b := make([]byte, csrfTokenLength)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}
