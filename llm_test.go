package main

import (
	"strings"
	"testing"
)

func TestApplyGlossaryOverrides(t *testing.T) {
	glossary := &LLMGlossary{
		Terms: []GlossaryTerm{
			{Phrase: "adom pending", Section: "Query Service"},
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
		{ID: 1, Description: "Investigate ADOM pending issue in QA"},
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

	_, userPrompt := buildSectionPrompts(cfg, options, items, existing)

	if !strings.Contains(userPrompt, "EX|S0_0|1234567890...") {
		t.Fatalf("expected first example to be truncated by max chars, prompt=%s", userPrompt)
	}
	if strings.Count(userPrompt, "EX|") != 1 {
		t.Fatalf("expected only one prompt example due to llm_example_count, prompt=%s", userPrompt)
	}
}
