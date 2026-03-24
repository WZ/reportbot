package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockValidateOK returns a valid session payload.
func mockValidateOK(userID, userName string) ValidateFunc {
	return func(cookie *http.Cookie) (SessionPayload, error) {
		return SessionPayload{UserID: userID, UserName: userName}, nil
	}
}

// mockValidateFail returns an error on validation.
func mockValidateFail() ValidateFunc {
	return func(cookie *http.Cookie) (SessionPayload, error) {
		return SessionPayload{}, http.ErrNoCookie
	}
}

// mockClearCookie returns a cookie that clears the session.
func mockClearCookie() ClearCookieFunc {
	return func() *http.Cookie {
		return &http.Cookie{
			Name:   "reportbot_session",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		}
	}
}

// mockIsManager returns true if userID matches the given managerID.
func mockIsManager(managerID string) IsManagerFunc {
	return func(userID string) bool {
		return userID == managerID
	}
}

// validCookie returns a cookie with the session name.
func validCookie() *http.Cookie {
	return &http.Cookie{
		Name:    "reportbot_session",
		Value:   "test-session-value",
		Path:    "/",
		Expires: time.Now().Add(24 * time.Hour),
	}
}

func TestAuthMiddleware_ValidCookie(t *testing.T) {
	handlerCalled := false
	var capturedUserID, capturedUserName string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		capturedUserID = UserID(r)
		capturedUserName = UserName(r)
		w.WriteHeader(http.StatusOK)
	})

	middleware := Auth(
		mockValidateOK("U123", "alice"),
		mockClearCookie(),
		mockIsManager("UMGR"),
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(validCookie())
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if !handlerCalled {
		t.Fatal("expected inner handler to be called")
	}
	if capturedUserID != "U123" {
		t.Errorf("expected UserID 'U123', got %q", capturedUserID)
	}
	if capturedUserName != "alice" {
		t.Errorf("expected UserName 'alice', got %q", capturedUserName)
	}
}

func TestAuthMiddleware_ExpiredCookie(t *testing.T) {
	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	middleware := Auth(
		mockValidateFail(),
		mockClearCookie(),
		mockIsManager(""),
	)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.AddCookie(validCookie())
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if handlerCalled {
		t.Fatal("expected inner handler NOT to be called for expired cookie")
	}
	if rr.Code != http.StatusFound {
		t.Errorf("expected status %d, got %d", http.StatusFound, rr.Code)
	}
	loc := rr.Header().Get("Location")
	if loc != "/login" {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestAuthMiddleware_MissingCookie(t *testing.T) {
	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	middleware := Auth(
		mockValidateOK("U123", "alice"),
		mockClearCookie(),
		mockIsManager(""),
	)

	req := httptest.NewRequest("GET", "/protected", nil)
	// No cookie added
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if handlerCalled {
		t.Fatal("expected inner handler NOT to be called without cookie")
	}
	if rr.Code != http.StatusFound {
		t.Errorf("expected status %d, got %d", http.StatusFound, rr.Code)
	}
	loc := rr.Header().Get("Location")
	if loc != "/login" {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestAuthMiddleware_TamperedCookie(t *testing.T) {
	handlerCalled := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
	})

	middleware := Auth(
		mockValidateFail(), // simulate HMAC mismatch
		mockClearCookie(),
		mockIsManager(""),
	)

	req := httptest.NewRequest("GET", "/protected", nil)
	req.AddCookie(&http.Cookie{
		Name:  "reportbot_session",
		Value: "tampered-value",
	})
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if handlerCalled {
		t.Fatal("expected inner handler NOT to be called for tampered cookie")
	}
	if rr.Code != http.StatusFound {
		t.Errorf("expected status %d, got %d", http.StatusFound, rr.Code)
	}
	// Should set a clear cookie
	cookies := rr.Result().Cookies()
	foundClear := false
	for _, c := range cookies {
		if c.Name == "reportbot_session" && c.MaxAge == -1 {
			foundClear = true
		}
	}
	if !foundClear {
		t.Error("expected a session-clearing cookie to be set")
	}
}

func TestAuthMiddleware_ManagerRole(t *testing.T) {
	var capturedIsManager bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedIsManager = IsManager(r)
		w.WriteHeader(http.StatusOK)
	})

	middleware := Auth(
		mockValidateOK("UMGR", "manager-alice"),
		mockClearCookie(),
		mockIsManager("UMGR"), // UMGR is a manager
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(validCookie())
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !capturedIsManager {
		t.Error("expected IsManager to be true for manager user")
	}
}

func TestAuthMiddleware_NonManagerReadOnly(t *testing.T) {
	var capturedIsManager bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedIsManager = IsManager(r)
		w.WriteHeader(http.StatusOK)
	})

	middleware := Auth(
		mockValidateOK("UDEV1", "developer-bob"),
		mockClearCookie(),
		mockIsManager("UMGR"), // Only UMGR is a manager
	)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(validCookie())
	rr := httptest.NewRecorder()

	middleware(inner).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if capturedIsManager {
		t.Error("expected IsManager to be false for non-manager user")
	}
}
