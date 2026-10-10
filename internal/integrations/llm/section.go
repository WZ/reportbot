package llm

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"reportbot/internal/domain"
)

// UndeterminedID is the section_id the LLM returns when no Section fits.
const UndeterminedID = "UND"

// SectionKey is the prompt's stable ID for a Section.
func SectionKey(id int64) string { return "S" + strconv.FormatInt(id, 10) }

func parseSectionKey(key string) (int64, bool) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "S") {
		return 0, false
	}
	id, err := strconv.ParseInt(key[1:], 10, 64)
	return id, err == nil && id > 0
}

// Example is a similar past Work Item and the Section it was filed in.
type Example struct {
	SectionID   int64
	Author      string
	Description string
}

// PromptItem is one Work Item to file.
type PromptItem struct {
	ID      int64
	Author  string
	Source  string
	Text    string
	Similar []Example
}

// CorrectionLine is a person moving a Work Item away from the LLM's pick.
type CorrectionLine struct {
	Description string
	From        int64
	To          int64
}

func sourceLabel(source string) string {
	switch source {
	case "gitlab", "github":
		return "merge request from code hosting"
	default:
		return "Slack report"
	}
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// BuildSectionPrompts builds the system prompt (stable for a Taxonomy, so it caches)
// and the user prompt for a batch of Work Items.
func BuildSectionPrompts(sections []domain.Section, recent map[int64][]Example, items []PromptItem, corrections []CorrectionLine, exampleMaxLen int) (string, string) {
	var secLines strings.Builder
	paths := make(map[int64]string, len(sections))
	for _, s := range sections {
		paths[s.ID] = s.Path()
		line := fmt.Sprintf("- %s: %s", SectionKey(s.ID), s.Path())
		if d := strings.TrimSpace(s.Description); d != "" {
			line += " — " + d
		}
		secLines.WriteString(line + "\n")
		for _, ex := range recent[s.ID] {
			secLines.WriteString(fmt.Sprintf("    recent: %s: %s\n", ex.Author, truncateRunes(ex.Description, exampleMaxLen)))
		}
	}

	system := `You file work items into the team's weekly status report. Each work item goes into exactly one section.

Sections (id: Area › Category › Section — description when the manager wrote one), each with items recently filed there:
` + secLines.String() + `
The team files a new item in the same section as earlier items about the same feature, component, release or customer.
Each work item comes with the most similar items the team filed before and where they were filed; follow those conventions.
If no section fits, use section_id "` + UndeterminedID + `".

Respond with JSON only: {"items": [{"id": <work item id>, "section_id": "<section id or ` + UndeterminedID + `>"}]}`

	var user strings.Builder
	user.WriteString("Recent corrections by people (the first section was wrong):\n")
	if len(corrections) == 0 {
		user.WriteString("none\n")
	}
	for _, c := range corrections {
		user.WriteString(fmt.Sprintf("- %q was filed in %s (%s), moved to %s (%s)\n",
			truncateRunes(c.Description, 120), SectionKey(c.From), paths[c.From], SectionKey(c.To), paths[c.To]))
	}
	for _, it := range items {
		user.WriteString(fmt.Sprintf("\nWork item %d\n  author: %s\n  source: %s\n  text: %s\n  similar past items (most similar first):\n",
			it.ID, it.Author, sourceLabel(it.Source), strings.TrimSpace(it.Text)))
		if len(it.Similar) == 0 {
			user.WriteString("  none\n")
		}
		for _, ex := range it.Similar {
			user.WriteString(fmt.Sprintf("  - %s | %s: %s\n", SectionKey(ex.SectionID), ex.Author, truncateRunes(ex.Description, exampleMaxLen)))
		}
	}
	return system, user.String()
}

// SectionSchema is the strict JSON schema for the OpenAI structured-output path.
func SectionSchema(sections []domain.Section) map[string]any {
	ids := make([]string, 0, len(sections)+1)
	ids = append(ids, UndeterminedID)
	for _, s := range sections {
		ids = append(ids, SectionKey(s.ID))
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"items"},
		"properties": map[string]any{
			"items": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"id", "section_id"},
					"properties": map[string]any{
						"id":         map[string]any{"type": "integer"},
						"section_id": map[string]any{"type": "string", "enum": ids},
					},
				},
			},
		},
	}
}

// extractJSONObject finds the outermost JSON object in text that may carry surrounding prose.
func extractJSONObject(text string) (string, bool) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return "", false
	}
	return text[start : end+1], true
}

// ParseSectionResponse maps item ID -> Section ID, 0 meaning Undetermined. Items the
// response omits are absent; an unknown section_id counts as Undetermined.
func ParseSectionResponse(text string, sections []domain.Section) (map[int64]int64, error) {
	obj, ok := extractJSONObject(text)
	if !ok {
		return nil, fmt.Errorf("no JSON object in LLM response")
	}
	var parsed struct {
		Items []struct {
			ID        int64  `json:"id"`
			SectionID string `json:"section_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(obj), &parsed); err != nil {
		return nil, fmt.Errorf("parsing LLM section response: %w", err)
	}
	valid := make(map[int64]bool, len(sections))
	for _, s := range sections {
		valid[s.ID] = true
	}
	out := make(map[int64]int64, len(parsed.Items))
	for _, it := range parsed.Items {
		id, ok := parseSectionKey(it.SectionID)
		if !ok || !valid[id] {
			id = 0
		}
		out[it.ID] = id
	}
	return out, nil
}

// SectionResult is the LLM's pick for a batch.
type SectionResult struct {
	Picks   map[int64]int64 // item ID -> Section ID (0 = Undetermined)
	Usage   LLMUsage
	Model   string
	RawText string
}

// ClassifySections asks the LLM for one Section per item.
func ClassifySections(cfg Config, sections []domain.Section, recent map[int64][]Example, items []PromptItem, corrections []CorrectionLine) (SectionResult, error) {
	system, user := BuildSectionPrompts(sections, recent, items, corrections, cfg.LLMExampleMaxLen)
	model := ModelName(cfg)
	res := SectionResult{Model: model}
	var err error
	log.Printf("llm section provider=%s model=%s items=%d sections=%d", cfg.LLMProvider, model, len(items), len(sections))
	switch cfg.LLMProvider {
	case "openai":
		res.RawText, res.Usage, err = callOpenAIStructured(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, system, user, "section_placement", SectionSchema(sections))
	default:
		res.RawText, res.Usage, err = callAnthropic(cfg.AnthropicAPIKey, model, system, user)
	}
	if err != nil {
		return res, err
	}
	res.Picks, err = ParseSectionResponse(res.RawText, sections)
	return res, err
}
