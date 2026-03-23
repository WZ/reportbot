package middleware

import (
	"context"
	"net/http"
)

type contextKey string

const (
	userIDKey    contextKey = "userID"
	userNameKey  contextKey = "userName"
	isManagerKey contextKey = "isManager"
)

// SessionPayload matches the web.SessionPayload structure.
type SessionPayload struct {
	UserID   string
	UserName string
}

// ValidateFunc validates a cookie and returns a session payload.
type ValidateFunc func(cookie *http.Cookie) (SessionPayload, error)

// ClearCookieFunc returns a cookie that clears the session.
type ClearCookieFunc func() *http.Cookie

// IsManagerFunc checks if a user ID is a manager.
type IsManagerFunc func(userID string) bool

// Auth checks for a valid session cookie and populates the request context.
// Functions are passed as parameters to avoid import cycle with the web package.
func Auth(validate ValidateFunc, clearCookie ClearCookieFunc, isManager IsManagerFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("reportbot_session")
			if err != nil {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			payload, err := validate(cookie)
			if err != nil {
				http.SetCookie(w, clearCookie())
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}

			// Derive role per-request so config changes take effect immediately
			manager := isManager(payload.UserID)

			ctx := r.Context()
			ctx = context.WithValue(ctx, userIDKey, payload.UserID)
			ctx = context.WithValue(ctx, userNameKey, payload.UserName)
			ctx = context.WithValue(ctx, isManagerKey, manager)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserID returns the authenticated user's Slack ID from the request context.
func UserID(r *http.Request) string {
	if v, ok := r.Context().Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

// UserName returns the authenticated user's name from the request context.
func UserName(r *http.Request) string {
	if v, ok := r.Context().Value(userNameKey).(string); ok {
		return v
	}
	return ""
}

// IsManager returns whether the authenticated user is a manager.
func IsManager(r *http.Request) bool {
	if v, ok := r.Context().Value(isManagerKey).(bool); ok {
		return v
	}
	return false
}

// RequireManager middleware rejects non-manager requests with 403.
func RequireManager(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsManager(r) {
			http.Error(w, "Forbidden: manager access required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
