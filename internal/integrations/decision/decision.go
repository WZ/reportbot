// Package decision calls a System One-protocol decision model: typed questions about
// a state, answered with calibrated probabilities.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"reportbot/internal/httpx"
)

// Question is one typed question. Instructions are always plain text: the gateway
// rejects structured instructions, so data goes in the state and is referenced by path.
type Question struct {
	Type         string `json:"type"` // "noul" | "choice" | "score"
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Raw           json.RawMessage    `json:"-"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Latency time.Duration     `json:"-"`
}

type Client struct {
	URL        string // base URL; the request goes to URL + "/v1/systemone"
	APIKey     string
	Model      string
	HTTPClient *http.Client
	MaxTries   int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	// sem bounds in-flight requests across the process.
	sem chan struct{}
}

// maxInFlight is the gateway's measured comfortable parallelism.
const maxInFlight = 8

const maxRetryAfter = 5 * time.Minute

func NewClient(url, apiKey, model string) *Client {
	return &Client{
		URL:        strings.TrimRight(url, "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTPClient: httpx.ExternalHTTPClient(),
		MaxTries:   5,
		BaseDelay:  2 * time.Second,
		MaxDelay:   60 * time.Second,
		sem:        make(chan struct{}, maxInFlight),
	}
}

// StatusError is a non-2xx gateway reply.
type StatusError struct {
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("decision model HTTP %d: %s", e.Code, e.Body)
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == 529 || code >= 500
}

// Ask sends one request with every question, retrying 429/5xx/529 and network errors.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (*Response, error) {
	if len(questions) == 0 {
		return &Response{Answers: map[string]Answer{}}, nil
	}
	body, err := json.Marshal(Request{Model: c.Model, State: state, Questions: questions})
	if err != nil {
		return nil, err
	}
	tries := c.MaxTries
	if tries < 1 {
		tries = 1
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			delay := c.backoff(attempt, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, err := c.do(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if se, ok := err.(*StatusError); ok && !retryable(se.Code) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("decision model unreachable after %d tries: %w", tries, lastErr)
}

func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	if se, ok := lastErr.(*StatusError); ok && se.RetryAfter > 0 {
		return min(se.RetryAfter, maxRetryAfter)
	}
	d := c.BaseDelay << (attempt - 1)
	if d > c.MaxDelay || d <= 0 {
		d = c.MaxDelay
	}
	return d
}

func (c *Client) do(ctx context.Context, body []byte) (*Response, error) {
	if c.sem != nil {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, &StatusError{Code: resp.StatusCode, Body: msg, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	var envelope struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   Usage                      `json:"usage"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decoding decision model response: %w", err)
	}
	out := &Response{Model: envelope.Model, Usage: envelope.Usage, Answers: map[string]Answer{}, Latency: time.Since(start)}
	for id, a := range envelope.Answers {
		var ans Answer
		if err := json.Unmarshal(a, &ans); err != nil {
			return nil, fmt.Errorf("decoding answer %q: %w", id, err)
		}
		ans.Raw = a
		out.Answers[id] = ans
	}
	return out, nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
