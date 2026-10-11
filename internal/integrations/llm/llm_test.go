package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reportbot/internal/domain"
)

var testSections = []domain.Section{
	{ID: 12, Area: "Product A", Category: "Release and Support", Name: "7.6.3"},
	{ID: 14, Area: "Product A", Category: "Engineering", Name: "Cluster Manager", Description: "Cluster lifecycle and upgrades"},
	{ID: 20, Area: "Product A", Category: "Infrastructure"},
}

func TestSuggestSectionDescriptionsOpenAI(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"[{\"section_id\":\"S20\",\"title\":\"t\",\"reasoning\":\"r\",\"description\":\"Racks and power\"}]"}]}],"usage":{"input_tokens":100,"output_tokens":9}}`))
	}))
	defer srv.Close()

	cfg := Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: srv.URL, LLMModel: "m"}
	corr := []CorrectionLine{{Description: "Rack power", From: 14, To: 20}, {Description: "Rack cabling", From: 14, To: 20}}
	got, usage, err := SuggestSectionDescriptions(cfg, testSections, corr)
	if err != nil || len(got) != 1 || got[0].SectionID != 20 || usage.InputTokens != 100 {
		t.Fatalf("unexpected suggestions %+v usage=%+v err=%v", got, usage, err)
	}
	if _, sent := gotBody["reasoning"]; sent {
		t.Fatalf("no reasoning effort configured, none sent: %v", gotBody["reasoning"])
	}
	if in, _ := gotBody["input"].(string); !strings.Contains(in, `"Rack power": S14 (Product A › Engineering › Cluster Manager) -> S20 (Product A › Infrastructure)`) {
		t.Fatalf("unexpected prompt: %s", in)
	}

	cfg.LLMReasoningEffort = "low"
	if _, _, err := SuggestSectionDescriptions(cfg, testSections, corr); err != nil {
		t.Fatal(err)
	}
	if r, _ := gotBody["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("expected reasoning effort low, got %v", gotBody["reasoning"])
	}
}

func TestSuggestSectionDescriptionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: srv.URL}
	if _, _, err := SuggestSectionDescriptions(cfg, testSections, []CorrectionLine{{Description: "x", From: 14, To: 20}}); err == nil {
		t.Fatal("expected an error when the LLM is unreachable")
	}
}

func TestParseRetroResponse(t *testing.T) {
	text := "```json\n" + `[{"section_id":"S14","title":"t","reasoning":"r","description":"Cluster lifecycle; hardware goes to Infrastructure"},
	{"section_id":"S99","description":"unknown section"},{"section_id":"S20","description":""}]` + "\n```"
	got, err := parseRetroResponse(text, testSections)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SectionID != 14 || !strings.HasPrefix(got[0].Description, "Cluster lifecycle") {
		t.Fatalf("unexpected suggestions: %+v", got)
	}
}

func TestExtractResponsesOutputText(t *testing.T) {
	resp := openAIResponsesResponse{
		Output: []struct {
			Type    string `json:"type"`
			Role    string `json:"role,omitempty"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content,omitempty"`
		}{
			{
				Type: "reasoning",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "reasoning_text", Text: "thinking"},
				},
			},
			{
				Type: "message",
				Role: "assistant",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "output_text", Text: `[{"id":1,"section_id":"S0_0","normalized_status":"done","ticket_ids":[],"duplicate_of":""}]`},
				},
			},
		},
	}

	got, err := extractResponsesOutputText(resp)
	if err != nil {
		t.Fatalf("extractResponsesOutputText error: %v", err)
	}
	if !strings.Contains(got, `"section_id":"S0_0"`) {
		t.Fatalf("unexpected extracted output: %q", got)
	}
}

func TestExtractResponsesOutputText_ReasoningWithOutputText(t *testing.T) {
	// Real-world case: reasoning output also uses "output_text" content type.
	// The extractor must skip reasoning and return the message output.
	resp := openAIResponsesResponse{
		Output: []struct {
			Type    string `json:"type"`
			Role    string `json:"role,omitempty"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content,omitempty"`
		}{
			{
				Type: "reasoning",
				Role: "assistant",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "output_text", Text: "Let me think about classifying these items..."},
				},
			},
			{
				Type: "message",
				Role: "assistant",
				Content: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{
					{Type: "output_text", Text: `[{"id":1,"section_id":"S7_0","normalized_status":"done","ticket_ids":[],"duplicate_of":""}]`},
				},
			},
		},
	}

	got, err := extractResponsesOutputText(resp)
	if err != nil {
		t.Fatalf("extractResponsesOutputText error: %v", err)
	}
	if !strings.HasPrefix(got, "[") {
		t.Fatalf("expected JSON array, got reasoning text: %q", got[:50])
	}
}
