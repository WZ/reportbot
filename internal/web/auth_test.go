package web

import (
	"net/http"
	"testing"
	"time"
)

func TestCreateSessionCookie(t *testing.T) {
	secret := "test-secret-key-1234"
	payload := SessionPayload{
		UserID:    "U12345",
		UserName:  "alice",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}

	cookie, err := CreateSessionCookie(secret, payload, false)
	if err != nil {
		t.Fatalf("CreateSessionCookie failed: %v", err)
	}

	if cookie.Name != "reportbot_session" {
		t.Errorf("expected cookie name 'reportbot_session', got %q", cookie.Name)
	}
	if cookie.Path != "/" {
		t.Errorf("expected cookie path '/', got %q", cookie.Path)
	}
	if !cookie.HttpOnly {
		t.Error("expected cookie to be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("expected SameSiteLaxMode, got %v", cookie.SameSite)
	}
	if cookie.Value == "" {
		t.Error("expected non-empty cookie value")
	}
	// Value should contain a dot separating payload and signature
	found := false
	for _, c := range cookie.Value {
		if c == '.' {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected cookie value to contain a '.' separator between payload and signature")
	}
}

func TestValidateSessionCookie(t *testing.T) {
	secret := "test-secret-key-roundtrip"
	payload := SessionPayload{
		UserID:    "U99999",
		UserName:  "bob",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	cookie, err := CreateSessionCookie(secret, payload, false)
	if err != nil {
		t.Fatalf("CreateSessionCookie failed: %v", err)
	}

	got, err := ValidateSessionCookie(secret, cookie)
	if err != nil {
		t.Fatalf("ValidateSessionCookie failed: %v", err)
	}

	if got.UserID != "U99999" {
		t.Errorf("expected UserID 'U99999', got %q", got.UserID)
	}
	if got.UserName != "bob" {
		t.Errorf("expected UserName 'bob', got %q", got.UserName)
	}
}

func TestSessionCookieExpiry(t *testing.T) {
	secret := "test-secret-key-expiry"
	payload := SessionPayload{
		UserID:    "UEXPIRED",
		UserName:  "expired-user",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // already expired
	}

	cookie, err := CreateSessionCookie(secret, payload, false)
	if err != nil {
		t.Fatalf("CreateSessionCookie failed: %v", err)
	}

	_, err = ValidateSessionCookie(secret, cookie)
	if err == nil {
		t.Fatal("expected ValidateSessionCookie to fail for expired session, but got nil error")
	}
}

func TestSessionCookieHMACIntegrity(t *testing.T) {
	secret := "test-secret-key-hmac"
	payload := SessionPayload{
		UserID:    "UTAMPER",
		UserName:  "tamper-user",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	cookie, err := CreateSessionCookie(secret, payload, false)
	if err != nil {
		t.Fatalf("CreateSessionCookie failed: %v", err)
	}

	// Tamper with the cookie value by flipping a character in the signature portion
	original := cookie.Value
	tampered := original[:len(original)-1]
	lastChar := original[len(original)-1]
	if lastChar == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	cookie.Value = tampered

	_, err = ValidateSessionCookie(secret, cookie)
	if err == nil {
		t.Fatal("expected ValidateSessionCookie to fail for tampered cookie, but got nil error")
	}

	// Also test with a completely wrong secret
	cookie.Value = original
	_, err = ValidateSessionCookie("wrong-secret", cookie)
	if err == nil {
		t.Fatal("expected ValidateSessionCookie to fail with wrong secret, but got nil error")
	}
}
