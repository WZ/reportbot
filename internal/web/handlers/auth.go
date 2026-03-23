package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reportbot/internal/web"
	"strings"
	"time"
)

// LoginPage renders the Slack OAuth login page.
func LoginPage(cfg web.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := web.GenerateOAuthState()
		if err != nil {
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		// Store state in a short-lived cookie for validation on callback
		http.SetCookie(w, &http.Cookie{
			Name:     "oauth_state",
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   300, // 5 minutes
		})

		redirectURI := strings.TrimRight(cfg.WebBaseURL, "/") + "/auth/slack/callback"
		oauthURL := fmt.Sprintf(
			"https://slack.com/oauth/authorize?scope=identity.basic,identity.avatar&client_id=%s&redirect_uri=%s&state=%s",
			url.QueryEscape(cfg.WebClientID),
			url.QueryEscape(redirectURI),
			url.QueryEscape(state),
		)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>ReportBot - Sign In</title>
<link rel="stylesheet" href="/static/style.css"/>
</head><body>
<div style="display:flex;justify-content:center;align-items:center;min-height:80vh;flex-direction:column;">
<h1>ReportBot</h1>
<p>Sign in with your Slack account to access the report editor.</p>
<a href="%s" style="display:inline-block;padding:12px 24px;background:#1a1a1a;color:#fff;text-decoration:none;border-radius:6px;font-weight:500;">Sign in with Slack</a>
</div></body></html>`, oauthURL)
	}
}

// SlackOAuthCallback handles the OAuth redirect from Slack.
func SlackOAuthCallback(cfg web.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Validate state parameter
		stateCookie, err := r.Cookie("oauth_state")
		if err != nil || stateCookie.Value == "" {
			http.Error(w, "Missing OAuth state", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("state") != stateCookie.Value {
			http.Error(w, "Invalid OAuth state", http.StatusForbidden)
			return
		}
		// Clear the state cookie
		http.SetCookie(w, &http.Cookie{Name: "oauth_state", Path: "/", MaxAge: -1})

		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}

		// Exchange code for access token
		redirectURI := strings.TrimRight(cfg.WebBaseURL, "/") + "/auth/slack/callback"
		resp, err := http.PostForm("https://slack.com/api/oauth.access", url.Values{
			"client_id":     {cfg.WebClientID},
			"client_secret": {cfg.WebClientSecret},
			"code":          {code},
			"redirect_uri":  {redirectURI},
		})
		if err != nil {
			log.Printf("OAuth token exchange failed: %v", err)
			http.Error(w, "Authentication failed", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var oauthResp struct {
			OK   bool `json:"ok"`
			User struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&oauthResp); err != nil {
			log.Printf("OAuth response decode failed: %v", err)
			http.Error(w, "Authentication failed", http.StatusInternalServerError)
			return
		}
		if !oauthResp.OK {
			log.Printf("Slack OAuth error: %s", oauthResp.Error)
			http.Error(w, "Slack authentication failed: "+oauthResp.Error, http.StatusForbidden)
			return
		}

		// Create session cookie
		cookie, err := web.CreateSessionCookie(cfg.WebSessionSecret, web.SessionPayload{
			UserID:    oauthResp.User.ID,
			UserName:  oauthResp.User.Name,
			ExpiresAt: time.Now().Add(24 * time.Hour),
		})
		if err != nil {
			log.Printf("Session cookie creation failed: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, cookie)
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

// Logout clears the session cookie.
func Logout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, web.ClearSessionCookie())
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
