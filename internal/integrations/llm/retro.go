package llm

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"reportbot/internal/domain"
)

// RetroSuggestion proposes a one-line description for a Section people keep correcting.
type RetroSuggestion struct {
	SectionID   int64
	Title       string
	Reasoning   string
	Description string
}

const maxRetroSuggestions = 5

// SuggestSectionDescriptions analyses recent corrections and proposes better one-line
// Section descriptions, which both models then see.
func SuggestSectionDescriptions(cfg Config, sections []domain.Section, corrections []CorrectionLine) ([]RetroSuggestion, LLMUsage, error) {
	if len(corrections) == 0 {
		return nil, LLMUsage{}, nil
	}
	system, user := buildRetroPrompts(sections, corrections)
	model := ModelName(cfg)
	log.Printf("llm retrospective provider=%s model=%s corrections=%d", cfg.LLMProvider, model, len(corrections))
	var text string
	var usage LLMUsage
	var err error
	switch cfg.LLMProvider {
	case "openai":
		text, usage, err = callOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, cfg.LLMReasoningEffort, system, user)
	default:
		text, usage, err = callAnthropic(cfg.AnthropicAPIKey, model, system, user)
	}
	if err != nil {
		return nil, usage, err
	}
	out, err := parseRetroResponse(text, sections)
	return out, usage, err
}

func buildRetroPrompts(sections []domain.Section, corrections []CorrectionLine) (string, string) {
	var secLines strings.Builder
	paths := make(map[int64]string, len(sections))
	for _, s := range sections {
		paths[s.ID] = s.Path()
		line := fmt.Sprintf("- %s: %s", SectionKey(s.ID), s.Path())
		if d := strings.TrimSpace(s.Description); d != "" {
			line += " — current description: " + d
		}
		secLines.WriteString(line + "\n")
	}
	system := `You help a team lead keep a weekly status report's sections clear.

Sections:
` + secLines.String() + `
Below are work items that people moved out of the section an AI filed them in.
Find sections that people keep correcting (2 or more corrections into or out of the same section).
For each, write a one-line description (under 200 characters) for the section that would have
filed these items correctly: what belongs there, and what belongs in the neighbouring section instead.
At most 5 suggestions. In "title" and "reasoning", name sections by their path, never by id.

Respond with JSON only (no markdown):
[{"section_id": "S12", "title": "...", "reasoning": "...", "description": "..."}]`

	var user strings.Builder
	user.WriteString("Recent corrections:\n")
	for _, c := range corrections {
		user.WriteString(fmt.Sprintf("- %q: %s (%s) -> %s (%s)\n",
			truncateRunes(c.Description, 150), SectionKey(c.From), paths[c.From], SectionKey(c.To), paths[c.To]))
	}
	return system, user.String()
}

func parseRetroResponse(text string, sections []domain.Section) ([]RetroSuggestion, error) {
	text = strings.TrimSpace(text)
	start, end := strings.Index(text, "["), strings.LastIndex(text, "]")
	if start == -1 || end <= start {
		return nil, fmt.Errorf("no JSON array in retrospective response")
	}
	var raw []struct {
		SectionID   string `json:"section_id"`
		Title       string `json:"title"`
		Reasoning   string `json:"reasoning"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("parsing retrospective response: %w", err)
	}
	valid := make(map[int64]bool, len(sections))
	for _, s := range sections {
		valid[s.ID] = true
	}
	var out []RetroSuggestion
	for _, r := range raw {
		id, ok := parseSectionKey(r.SectionID)
		desc := strings.TrimSpace(r.Description)
		if !ok || !valid[id] || desc == "" {
			continue
		}
		out = append(out, RetroSuggestion{SectionID: id, Title: strings.TrimSpace(r.Title), Reasoning: strings.TrimSpace(r.Reasoning), Description: desc})
		if len(out) == maxRetroSuggestions {
			break
		}
	}
	return out, nil
}
