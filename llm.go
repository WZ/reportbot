package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type sectionClassifiedItem struct {
	ID               int64   `json:"id"`
	SectionID        string  `json:"section_id"`
	NormalizedStatus string  `json:"normalized_status"`
	TicketIDs        string  `json:"ticket_ids"`
	DuplicateOf      string  `json:"duplicate_of"`
	Confidence       float64 `json:"confidence"`
}

type existingItemContext struct {
	Key         string
	SectionID   string
	Description string
	Status      string
}

type LLMSectionDecision struct {
	SectionID        string
	NormalizedStatus string
	TicketIDs        string
	DuplicateOf      string
	Confidence       float64
}

type LLMUsage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u LLMUsage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}

func (u *LLMUsage) Add(other LLMUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
}

const defaultAnthropicModel = "claude-sonnet-4-5-20250929"
const defaultOpenAIModel = "gpt-4o-mini"

func CategorizeItemsToSections(
	cfg Config,
	items []WorkItem,
	options []sectionOption,
	existing []existingItemContext,
) (map[int64]LLMSectionDecision, LLMUsage, error) {
	if len(items) == 0 {
		return nil, LLMUsage{}, nil
	}

	batchSize := cfg.LLMBatchSize
	if batchSize < 1 {
		batchSize = 50
	}
	all := make(map[int64]LLMSectionDecision, len(items))
	totalUsage := LLMUsage{}
	glossary, err := loadGlossaryIfConfigured(cfg)
	if err != nil {
		return nil, LLMUsage{}, err
	}
	glossarySectionMap := resolveGlossarySectionMap(glossary, options)

	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		systemPrompt, userPrompt := buildSectionPrompts(cfg, options, batch, existing)

		var responseText string
		var usage LLMUsage
		var err error

		switch cfg.LLMProvider {
		case "openai":
			model := cfg.LLMModel
			if model == "" {
				model = defaultOpenAIModel
			}
			log.Printf("llm section-classify provider=openai model=%s items=%d sections=%d batch=%d-%d", model, len(batch), len(options), start, end)
			responseText, usage, err = callOpenAI(cfg.OpenAIAPIKey, model, systemPrompt, userPrompt)
		default:
			model := cfg.LLMModel
			if model == "" {
				model = defaultAnthropicModel
			}
			log.Printf("llm section-classify provider=anthropic model=%s items=%d sections=%d batch=%d-%d", model, len(batch), len(options), start, end)
			responseText, usage, err = callAnthropic(cfg.AnthropicAPIKey, model, systemPrompt, userPrompt)
		}
		if err != nil {
			return nil, totalUsage, err
		}
		totalUsage.Add(usage)

		parsed, err := parseSectionClassifiedResponse(responseText)
		if err != nil {
			return nil, totalUsage, err
		}
		applyGlossaryOverrides(batch, parsed, glossary, glossarySectionMap)
		for id, decision := range parsed {
			all[id] = decision
		}
	}

	return all, totalUsage, nil
}

func buildSectionPrompts(cfg Config, options []sectionOption, items []WorkItem, existing []existingItemContext) (string, string) {
	var sectionLines strings.Builder
	for _, option := range options {
		sectionLines.WriteString(fmt.Sprintf("- %s: %s\n", option.ID, option.Label))
	}

	var itemLines strings.Builder
	for _, item := range items {
		itemLines.WriteString(fmt.Sprintf("ID:%d - %s (status: %s)\n", item.ID, strings.TrimSpace(item.Description), normalizeStatus(item.Status)))
	}

	var existingLines strings.Builder
	for _, ex := range existing {
		existingLines.WriteString(fmt.Sprintf("- %s | %s | %s | %s\n", ex.Key, ex.SectionID, strings.TrimSpace(ex.Status), strings.TrimSpace(ex.Description)))
	}

	existingBlock := "none"
	if existingLines.Len() > 0 {
		existingBlock = existingLines.String()
	}

	examplesBlock := "none"
	if len(existing) > 0 {
		exampleCount := cfg.LLMExampleCount
		exampleMaxLen := cfg.LLMExampleMaxLen
		var examples strings.Builder
		for i, ex := range existing {
			if i >= exampleCount {
				break
			}
			desc := strings.TrimSpace(ex.Description)
			if len(desc) > exampleMaxLen {
				desc = desc[:exampleMaxLen] + "..."
			}
			examples.WriteString(fmt.Sprintf("- EX|%s|%s\n", ex.SectionID, desc))
		}
		if examples.Len() > 0 {
			examplesBlock = examples.String()
		}
	}

	systemPrompt := fmt.Sprintf(`You classify software work items into one section.
Choose exactly one section_id for each item from:
%s

If none fit, use section_id "UND".
Also:
- choose normalized_status from: done, in testing, in progress, other
- extract ticket IDs if present (e.g. [1247202] or bare ticket numbers)
- if this item is the same underlying work as an existing item, set duplicate_of to that existing key (Kxx); otherwise empty string
- set confidence between 0 and 1.

Respond with JSON only (no markdown):
[{"id": 1, "section_id": "S0_2", "normalized_status": "in progress", "ticket_ids": "1247202", "duplicate_of": "K3", "confidence": 0.91}, ...]`, sectionLines.String())

	userPrompt := "Examples from previous reports:\n" + examplesBlock +
		"\nExisting items (for duplicate_of):\n" + existingBlock +
		"\nClassify these items:\n\n" + itemLines.String()
	return systemPrompt, userPrompt
}

func parseSectionClassifiedResponse(responseText string) (map[int64]LLMSectionDecision, error) {
	responseText = strings.TrimSpace(responseText)
	responseText = strings.TrimPrefix(responseText, "```json")
	responseText = strings.TrimPrefix(responseText, "```")
	responseText = strings.TrimSuffix(responseText, "```")
	responseText = strings.TrimSpace(responseText)

	var classified []sectionClassifiedItem
	if err := json.Unmarshal([]byte(responseText), &classified); err != nil {
		return nil, fmt.Errorf("parsing LLM section response: %w (response: %s)", err, responseText)
	}

	decisions := make(map[int64]LLMSectionDecision)
	for _, c := range classified {
		decisions[c.ID] = LLMSectionDecision{
			SectionID:        strings.TrimSpace(c.SectionID),
			NormalizedStatus: normalizeStatus(strings.TrimSpace(c.NormalizedStatus)),
			TicketIDs:        strings.TrimSpace(c.TicketIDs),
			DuplicateOf:      strings.TrimSpace(c.DuplicateOf),
			Confidence:       c.Confidence,
		}
	}
	return decisions, nil
}

func loadGlossaryIfConfigured(cfg Config) (*LLMGlossary, error) {
	if strings.TrimSpace(cfg.LLMGlossaryPath) == "" {
		return nil, nil
	}
	return LoadLLMGlossary(cfg.LLMGlossaryPath)
}

func resolveGlossarySectionMap(glossary *LLMGlossary, options []sectionOption) map[string]string {
	out := make(map[string]string)
	if glossary == nil {
		return out
	}

	byID := make(map[string]string)
	byLabel := make(map[string]string)
	for _, option := range options {
		byID[normalizeTextToken(option.ID)] = option.ID
		byLabel[normalizeTextToken(option.Label)] = option.ID
		parts := strings.Split(option.Label, ">")
		if len(parts) == 2 {
			byLabel[normalizeTextToken(parts[0])] = option.ID
			byLabel[normalizeTextToken(parts[1])] = option.ID
		}
	}

	for _, term := range glossary.Terms {
		target := normalizeTextToken(term.Section)
		switch {
		case byID[target] != "":
			out[normalizeTextToken(term.Phrase)] = byID[target]
		case byLabel[target] != "":
			out[normalizeTextToken(term.Phrase)] = byLabel[target]
		}
	}
	return out
}

func applyGlossaryOverrides(
	items []WorkItem,
	decisions map[int64]LLMSectionDecision,
	glossary *LLMGlossary,
	glossarySectionMap map[string]string,
) {
	if glossary == nil {
		return
	}

	for _, item := range items {
		desc := normalizeTextToken(item.Description)
		decision := decisions[item.ID]

		for phrase, sectionID := range glossarySectionMap {
			if phrase != "" && strings.Contains(desc, phrase) {
				decision.SectionID = sectionID
				if decision.Confidence < 0.99 {
					decision.Confidence = 0.99
				}
				break
			}
		}

		for _, hint := range glossary.StatusHints {
			phrase := normalizeTextToken(hint.Phrase)
			if phrase != "" && strings.Contains(desc, phrase) {
				decision.NormalizedStatus = normalizeStatus(hint.Status)
				break
			}
		}

		decisions[item.ID] = decision
	}
}

// --- Anthropic ---

func callAnthropic(apiKey, model, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	message, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 4096,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
		},
	})
	if err != nil {
		log.Printf("llm anthropic error: %v", err)
		return "", LLMUsage{}, fmt.Errorf("Anthropic API error: %w", err)
	}
	usage := LLMUsage{
		InputTokens:  message.Usage.InputTokens,
		OutputTokens: message.Usage.OutputTokens,
	}

	for _, block := range message.Content {
		if block.Type == "text" {
			log.Printf("llm anthropic response size=%d tokens_in=%d tokens_out=%d", len(block.Text), usage.InputTokens, usage.OutputTokens)
			return block.Text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("no text content in Anthropic response")
}

// --- OpenAI ---

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func callOpenAI(apiKey, model, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	reqBody := openAIRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("llm openai error: %v", err)
		return "", LLMUsage{}, fmt.Errorf("OpenAI API error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("reading response: %w", err)
	}

	var openAIResp openAIResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return "", LLMUsage{}, fmt.Errorf("parsing OpenAI response: %w", err)
	}

	if openAIResp.Error != nil {
		log.Printf("llm openai api error: %s", openAIResp.Error.Message)
		return "", LLMUsage{}, fmt.Errorf("OpenAI API error: %s", openAIResp.Error.Message)
	}

	if len(openAIResp.Choices) == 0 {
		return "", LLMUsage{}, fmt.Errorf("no choices in OpenAI response")
	}
	usage := LLMUsage{}
	if openAIResp.Usage != nil {
		usage.InputTokens = openAIResp.Usage.PromptTokens
		usage.OutputTokens = openAIResp.Usage.CompletionTokens
	}

	log.Printf("llm openai response size=%d tokens_in=%d tokens_out=%d", len(openAIResp.Choices[0].Message.Content), usage.InputTokens, usage.OutputTokens)
	return openAIResp.Choices[0].Message.Content, usage, nil
}
