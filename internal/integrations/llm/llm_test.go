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

	applyGlossaryOverrides(items, decisions, glossary, sectionMap)
	got := decisions[1]

	if got.SectionID != "S1_0" {
		t.Fatalf("expected glossary to override section to S1_0, got %s", got.SectionID)
	}
	if got.NormalizedStatus != "in testing" {
		t.Fatalf("expected glossary status override to in testing, got %s", got.NormalizedStatus)
	}
	if got.Confidence < 0.99 {
		t.Fatalf("expected glossary override to raise confidence, got %f", got.Confidence)
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
		{"id": 1, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": [], "duplicate_of": "", "confidence": 0.9},
		{"id": 2, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": ["1136790"], "duplicate_of": "", "confidence": 0.9},
		{"id": 3, "section_id": "S0_0", "normalized_status": "in progress", "ticket_ids": "1247202", "duplicate_of": "", "confidence": 0.9}
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
