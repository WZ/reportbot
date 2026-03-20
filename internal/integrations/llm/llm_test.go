package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLLMBatchConcurrencyLimit(t *testing.T) {
	tests := []struct {
		total int
		want  int
	}{
		{total: 0, want: 1},
		{total: 1, want: 1},
		{total: 2, want: 2},
		{total: 4, want: 4},
		{total: 10, want: 4},
	}
	for _, tt := range tests {
		if got := llmBatchConcurrencyLimit(tt.total); got != tt.want {
			t.Fatalf("llmBatchConcurrencyLimit(%d) = %d, want %d", tt.total, got, tt.want)
		}
	}
}

func TestApplyGlossaryOverrides(t *testing.T) {
	glossary := &LLMGlossary{
		Terms: []GlossaryTerm{
			{Phrase: "tenant pending", Section: "Query Service"},
		},
		StatusHints: []GlossaryStatusHint{
			{Phrase: "in qa", Status: "in testing"},
		},
	}
	options := []sectionOption{
		{ID: "S0_0", Label: "Infrastructure"},
		{ID: "S1_0", Label: "Query Service"},
	}
	sectionMap := resolveGlossarySectionMap(glossary, options)

	items := []WorkItem{
		{ID: 1, Description: "Investigate tenant pending issue in QA"},
	}
	decisions := map[int64]LLMSectionDecision{
		1: {SectionID: "S0_0", Confidence: 0.20},
	}

	overrides := applyGlossaryOverrides(items, decisions, glossary, sectionMap)
	assignLocalConfidence(decisions, options, overrides)
	got := decisions[1]

	if got.SectionID != "S1_0" {
		t.Fatalf("expected glossary to override section to S1_0, got %s", got.SectionID)
	}
	if got.NormalizedStatus != "in testing" {
		t.Fatalf("expected glossary status override to in testing, got %s", got.NormalizedStatus)
	}
	if got.Confidence != 0.99 {
		t.Fatalf("expected glossary override confidence 0.99, got %f", got.Confidence)
	}
}

func TestBuildSectionPrompts_UsesExampleLimits(t *testing.T) {
	cfg := Config{
		LLMExampleCount:  1,
		LLMExampleMaxLen: 10,
	}
	options := []sectionOption{
		{ID: "S0_0", Label: "Top Focus"},
	}
	items := []WorkItem{
		{ID: 1, Description: "Current item"},
	}
	existing := []existingItemContext{
		{SectionID: "S0_0", Description: "123456789012345"},
		{SectionID: "S0_0", Description: "second example"},
	}

	_, userPrompt := buildSectionPrompts(cfg, options, items, existing, "", nil, nil)

	if !strings.Contains(userPrompt, "EX|S0_0|1234567890...") {
		t.Fatalf("expected first example to be truncated by max chars, prompt=%s", userPrompt)
	}
	if strings.Count(userPrompt, "EX|") != 1 {
		t.Fatalf("expected only one prompt example due to llm_example_count, prompt=%s", userPrompt)
	}
}

func TestBuildSectionPrompts_IncludesTemplateGuidance(t *testing.T) {
	cfg := Config{
		LLMExampleCount:  1,
		LLMExampleMaxLen: 100,
	}
	options := []sectionOption{
		{ID: "S0_0", Label: "Top Focus"},
	}
	items := []WorkItem{
		{ID: 1, Description: "Current item"},
	}

	systemPrompt, _ := buildSectionPrompts(cfg, options, items, nil, "Rule: prefer query section for database items", nil, nil)
	if !strings.Contains(systemPrompt, "Template guidance") {
		t.Fatalf("expected template guidance marker in system prompt")
	}
	if !strings.Contains(systemPrompt, "prefer query section for database items") {
		t.Fatalf("expected template guidance content in system prompt")
	}
}

func TestParseSectionClassifiedResponse_AcceptsArrayTicketIDs(t *testing.T) {
	response := `[
		{"id": 1, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": [], "duplicate_of": ""},
		{"id": 2, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": ["1136790"], "duplicate_of": ""},
		{"id": 3, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": "1247202", "duplicate_of": ""}
	]`

	got, err := parseSectionClassifiedResponse(response)
	if err != nil {
		t.Fatalf("parseSectionClassifiedResponse should accept array ticket_ids: %v", err)
	}
	if got[1].TicketIDs != "" {
		t.Fatalf("expected empty ticket IDs for [] , got %q", got[1].TicketIDs)
	}
	if got[2].TicketIDs != "1136790" {
		t.Fatalf("expected single ticket ID from array, got %q", got[2].TicketIDs)
	}
	if got[3].TicketIDs != "1247202" {
		t.Fatalf("expected ticket ID from string, got %q", got[3].TicketIDs)
	}
}

func TestParseTicketIDsField_MixedArray(t *testing.T) {
	raw := json.RawMessage(`[ "123", 456, "", " 789 " ]`)
	got := parseTicketIDsField(raw)
	if got != "123,456,789" {
		t.Fatalf("unexpected ticket IDs normalization: %q", got)
	}
}

func TestAssignLocalConfidence(t *testing.T) {
	decisions := map[int64]LLMSectionDecision{
		1: {SectionID: "S0_0"},
		2: {SectionID: "UND"},
		3: {SectionID: "S0_0", DuplicateOf: "K2"},
		4: {SectionID: "UNKNOWN"},
		5: {SectionID: "und"},
	}
	options := []sectionOption{{ID: "S0_0", Label: "Query Service"}}
	assignLocalConfidence(decisions, options, map[int64]bool{1: true})

	if decisions[1].Confidence != 0.99 {
		t.Fatalf("expected glossary override confidence, got %v", decisions[1].Confidence)
	}
	if decisions[2].Confidence != 0.40 {
		t.Fatalf("expected UND confidence, got %v", decisions[2].Confidence)
	}
	if decisions[3].Confidence != 0.95 {
		t.Fatalf("expected duplicate confidence, got %v", decisions[3].Confidence)
	}
	if decisions[4].Confidence != 0.20 {
		t.Fatalf("expected invalid section confidence, got %v", decisions[4].Confidence)
	}
	if decisions[5].Confidence != 0.40 {
		t.Fatalf("expected lowercase und confidence, got %v", decisions[5].Confidence)
	}
	if decisions[5].SectionID != "UND" {
		t.Fatalf("expected lowercase und to normalize to UND, got %q", decisions[5].SectionID)
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

func TestParseCriticResponse(t *testing.T) {
	response := `[
		{"id": 42, "reason": "This is a database task not infra", "suggested_section_id": "S1_0"},
		{"id": 55, "reason": "Belongs to auth service", "suggested_section_id": "S2_1"}
	]`

	flagged, err := parseCriticResponse(response)
	if err != nil {
		t.Fatalf("parseCriticResponse error: %v", err)
	}
	if len(flagged) != 2 {
		t.Fatalf("expected 2 flagged items, got %d", len(flagged))
	}
	if flagged[0].ID != 42 || flagged[0].SuggestedSectionID != "S1_0" {
		t.Fatalf("unexpected first flagged item: %+v", flagged[0])
	}
	if flagged[1].ID != 55 || flagged[1].SuggestedSectionID != "S2_1" {
		t.Fatalf("unexpected second flagged item: %+v", flagged[1])
	}
}

func TestParseCriticResponse_Empty(t *testing.T) {
	flagged, err := parseCriticResponse("[]")
	if err != nil {
		t.Fatalf("parseCriticResponse error on empty: %v", err)
	}
	if len(flagged) != 0 {
		t.Fatalf("expected 0 flagged items, got %d", len(flagged))
	}
}

func TestExtractJSONArray(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"pure array", `[{"id":1}]`, `[{"id":1}]`, true},
		{"reasoning prefix", `Let me think about this.\n[{"id":1}]`, `[{"id":1}]`, true},
		{"reasoning both sides", `Some reasoning\n[{"id":1}]\nDone.`, `[{"id":1}]`, true},
		{"no array", `No JSON here`, "", false},
		{"only open bracket", `text [ but no close`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractJSONArray(tt.input)
			if ok != tt.ok {
				t.Fatalf("extractJSONArray ok=%v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("extractJSONArray=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSectionClassifiedResponse_WithReasoning(t *testing.T) {
	// Simulate a model that dumps chain-of-thought before JSON
	reasoning := `We need to classify these items. Let me think...
ID 228 goes to S7_0. ID 237 is UND.
[{"id":228,"section_id":"S7_0","normalized_status":"in progress","ticket_ids":[],"duplicate_of":""},{"id":237,"section_id":"UND","normalized_status":"done","ticket_ids":[],"duplicate_of":""}]
That should be correct.`
	decisions, err := parseSectionClassifiedResponse(reasoning)
	if err != nil {
		t.Fatalf("parseSectionClassifiedResponse with reasoning: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(decisions))
	}
	if decisions[228].SectionID != "S7_0" {
		t.Errorf("expected S7_0, got %q", decisions[228].SectionID)
	}
	if decisions[237].SectionID != "UND" {
		t.Errorf("expected UND, got %q", decisions[237].SectionID)
	}
}
