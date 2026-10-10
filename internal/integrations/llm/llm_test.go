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

func TestBuildSectionPrompts(t *testing.T) {
	items := []PromptItem{{
		ID: 7, Author: "Dev A", Source: "slack", Text: "Fix rebalance timeout",
		Similar: []Example{{SectionID: 14, Author: "Dev B", Description: strings.Repeat("x", 50)}},
	}}
	corrections := []CorrectionLine{{Description: "Shard move stuck", From: 14, To: 20}}
	recent := map[int64][]Example{20: {{SectionID: 20, Author: "Dev C", Description: "Rack cabling"}}}
	system, user := BuildSectionPrompts(testSections, recent, items, corrections, 20)

	for _, want := range []string{
		"- S12: Product A › Release and Support › 7.6.3\n",
		"- S14: Product A › Engineering › Cluster Manager — Cluster lifecycle and upgrades\n",
		"- S20: Product A › Infrastructure\n    recent: Dev C: Rack cabling\n",
		`use section_id "UND"`,
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q:\n%s", want, system)
		}
	}
	for _, want := range []string{
		`"Shard move stuck" was filed in S14 (Product A › Engineering › Cluster Manager), moved to S20 (Product A › Infrastructure)`,
		"Work item 7\n  author: Dev A\n  source: Slack report\n  text: Fix rebalance timeout",
		"  - S14 | Dev B: " + strings.Repeat("x", 20) + "...\n",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, user)
		}
	}
}

func TestBuildSectionPromptsNoCorrectionsOrExamples(t *testing.T) {
	_, user := BuildSectionPrompts(testSections, nil, []PromptItem{{ID: 1, Source: "gitlab", Text: "x"}}, nil, 100)
	if !strings.Contains(user, "Recent corrections by people (the first section was wrong):\nnone\n") {
		t.Errorf("expected 'none' corrections block:\n%s", user)
	}
	if !strings.Contains(user, "source: merge request from code hosting") || !strings.Contains(user, "  none\n") {
		t.Errorf("expected code-change source and no examples:\n%s", user)
	}
}

func TestSectionSchemaEnumsStableIDs(t *testing.T) {
	schema := SectionSchema(testSections)
	b, _ := json.Marshal(schema)
	if !strings.Contains(string(b), `"enum":["UND","S12","S14","S20"]`) {
		t.Fatalf("unexpected schema: %s", b)
	}
}

func TestParseSectionResponse(t *testing.T) {
	text := `Reasoning first... {"items": [{"id": 1, "section_id": "S14"}, {"id": 2, "section_id": "UND"}, {"id": 3, "section_id": "S999"}]}`
	got, err := ParseSectionResponse(text, testSections)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] != 14 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("unexpected picks: %v", got)
	}
	if _, ok := got[4]; ok {
		t.Fatalf("missing items must stay absent")
	}
	if _, err := ParseSectionResponse("no json here", testSections); err == nil {
		t.Fatal("expected error for unparsable response")
	}
}

func TestClassifySectionsOpenAIStructured(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"items\":[{\"id\":5,\"section_id\":\"S20\"}]}"}]}],"usage":{"input_tokens":100,"output_tokens":9}}`))
	}))
	defer srv.Close()

	cfg := Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: srv.URL, LLMModel: "m", LLMExampleMaxLen: 100}
	res, err := ClassifySections(cfg, testSections, nil, []PromptItem{{ID: 5, Text: "Rack power"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Picks[5] != 20 || res.Usage.InputTokens != 100 || res.Model != "m" {
		t.Fatalf("unexpected result: %+v", res)
	}
	format := gotBody["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true {
		t.Fatalf("expected strict json_schema format, got %v", format)
	}
	if _, sent := gotBody["reasoning"]; sent {
		t.Fatalf("no reasoning effort configured, none sent: %v", gotBody["reasoning"])
	}

	cfg.LLMReasoningEffort = "low"
	if _, err := ClassifySections(cfg, testSections, nil, []PromptItem{{ID: 5, Text: "Rack power"}}, nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := gotBody["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("expected reasoning effort low, got %v", gotBody["reasoning"])
	}
}

func TestClassifySectionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: srv.URL}
	if _, err := ClassifySections(cfg, testSections, nil, []PromptItem{{ID: 1, Text: "x"}}, nil); err == nil {
		t.Fatal("expected error when the LLM is unreachable")
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
