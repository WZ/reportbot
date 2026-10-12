package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *Client {
	c := NewClient(url, "key", "test-model")
	c.HTTPClient = http.DefaultClient
	c.BaseDelay = time.Millisecond
	c.MaxDelay = 5 * time.Millisecond
	return c
}

func TestAskSendsTextInstructionsAndParsesAnswers(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer key" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Write([]byte(`{"model":"m-1","answers":{"check":{"type":"noul","noul":0.91},
			"status":{"type":"choice","choice":"done","probabilities":{"done":0.8,"in_progress":0.2},"confidence":0.6}},
			"usage":{"input_tokens":120,"output_tokens":20}}`))
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL+"/").Ask(context.Background(), map[string]any{"work_item": "x"}, map[string]Question{
		"check":  {Type: "noul", Instructions: "Does `work_item` belong in `proposed_section`?"},
		"status": {Type: "choice", Instructions: "Which status?", Criteria: map[string]string{"done": "finished", "in_progress": "ongoing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Answers["check"].Noul != 0.91 || resp.Answers["status"].Choice != "done" || resp.Usage.InputTokens != 120 || resp.Model != "m-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(resp.Answers["check"].Raw) == 0 {
		t.Fatal("raw answer must be kept for storage")
	}
	q := body["questions"].(map[string]any)["check"].(map[string]any)
	if _, isText := q["instructions"].(string); !isText {
		t.Fatalf("instructions must be plain text, got %T", q["instructions"])
	}
	if body["model"] != "test-model" {
		t.Fatalf("unexpected model %v", body["model"])
	}
}

func TestAskRetriesOn429And5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case 2:
			http.Error(w, "overloaded", 529)
		case 3:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			w.Write([]byte(`{"answers":{"q":{"type":"noul","noul":0.4}}}`))
		}
	}))
	defer srv.Close()

	resp, err := testClient(srv.URL).Ask(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || resp.Answers["q"].Noul != 0.4 {
		t.Fatalf("calls=%d resp=%+v", calls, resp)
	}
}

func TestAskDoesNotRetryValidationErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"detail":"instructions must be text"}`, http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Ask(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "?"}})
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusUnprocessableEntity || calls != 1 {
		t.Fatalf("expected one 422 without retry, got calls=%d err=%v", calls, err)
	}
}

func TestAskGivesUpWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	c.MaxTries = 3
	if _, err := c.Ask(context.Background(), "s", map[string]Question{"q": {Type: "noul", Instructions: "?"}}); err == nil {
		t.Fatal("expected an error after retries")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if parseRetryAfter("2") != 2*time.Second || parseRetryAfter("") != 0 || parseRetryAfter("junk") != 0 {
		t.Fatal("unexpected retry-after parsing")
	}
}
