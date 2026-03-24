package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const sessionCookieName = "reportbot_session"
const sessionDuration = 24 * time.Hour

// SessionPayload is the data stored in the session cookie.
type SessionPayload struct {
	UserID    string    `json:"uid"`
	UserName  string    `json:"name"`
	ExpiresAt time.Time `json:"exp"`
}

// CreateSessionCookie creates an HMAC-signed session cookie.
// Set secure=true when serving over HTTPS (derived from WebBaseURL scheme).
func CreateSessionCookie(secret string, payload SessionPayload, secure bool) (*http.Cookie, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal session: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	sig := mac.Sum(nil)

	value := base64.URLEncoding.EncodeToString(data) + "." + base64.URLEncoding.EncodeToString(sig)

	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(sessionDuration.Seconds()),
	}, nil
}

// ValidateSessionCookie validates an HMAC-signed session cookie and returns the payload.
func ValidateSessionCookie(secret string, cookie *http.Cookie) (SessionPayload, error) {
	var payload SessionPayload

	parts := splitCookieValue(cookie.Value)
	if len(parts) != 2 {
		return payload, fmt.Errorf("invalid cookie format")
	}

	data, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil {
		return payload, fmt.Errorf("decode payload: %w", err)
	}

	sig, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		return payload, fmt.Errorf("decode signature: %w", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return payload, fmt.Errorf("invalid signature")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return payload, fmt.Errorf("unmarshal session: %w", err)
	}

	if time.Now().After(payload.ExpiresAt) {
		return payload, fmt.Errorf("session expired")
	}

	return payload, nil
}

// ClearSessionCookie returns a cookie that clears the session.
func ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	}
}

// GenerateOAuthState creates a random state parameter for OAuth CSRF protection.
func GenerateOAuthState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func splitCookieValue(v string) []string {
	for i := len(v) - 1; i >= 0; i-- {
		if v[i] == '.' {
			return []string{v[:i], v[i+1:]}
		}
	}
	return []string{v}
}
