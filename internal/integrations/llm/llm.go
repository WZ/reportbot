package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type sectionClassifiedItem struct {
	ID               int64           `json:"id"`
	SectionID        string          `json:"section_id"`
	NormalizedStatus string          `json:"normalized_status"`
	TicketIDs        json.RawMessage `json:"ticket_ids"`
	DuplicateOf      string          `json:"duplicate_of"`
}

type ExistingItemContext struct {
	Key         string
	SectionID   string
	Description string
	Status      string
}

type existingItemContext = ExistingItemContext

type LLMSectionDecision struct {
	SectionID        string
	NormalizedStatus string
	TicketIDs        string
	DuplicateOf      string
	Confidence       float64
}

type LLMUsage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

func (u LLMUsage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens
}

func (u *LLMUsage) Add(other LLMUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
}

const defaultAnthropicModel = "claude-sonnet-4-5-20250929"
const defaultOpenAIModel = "gpt-4o-mini"
const maxTemplateGuidanceChars = 8000
const defaultLLMMaxConcurrentBatches = 4

func llmBatchConcurrencyLimit(totalBatches int) int {
	if totalBatches <= 0 {
		return 1
	}
	if totalBatches < defaultLLMMaxConcurrentBatches {
		return totalBatches
	}
	return defaultLLMMaxConcurrentBatches
}

func CategorizeItemsToSections(
	cfg Config,
	items []WorkItem,
	options []sectionOption,
	existing []existingItemContext,
	corrections []ClassificationCorrection,
	historicalItems []historicalItem,
) (map[int64]LLMSectionDecision, LLMUsage, error) {
	if len(items) == 0 {
		return nil, LLMUsage{}, nil
	}

	batchSize := cfg.LLMBatchSize
	if batchSize < 1 {
		batchSize = 50
	}
	glossary, err := loadGlossaryIfConfigured(cfg)
	if err != nil {
		return nil, LLMUsage{}, err
	}
	glossarySectionMap := resolveGlossarySectionMap(glossary, options)
	templateGuidance := loadTemplateGuidance(cfg.LLMGuidePath)

	// Build TF-IDF index for example selection.
	var tfidfIdx *tfidfIndex
	if len(historicalItems) > 0 {
		tfidfIdx = buildTFIDFIndex(historicalItems)
	}

	// Pre-slice all batches.
	var batches [][]WorkItem
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}

	type batchResult struct {
		decisions map[int64]LLMSectionDecision
		usage     LLMUsage
		err       error
	}
	results := make([]batchResult, len(batches))
	sem := make(chan struct{}, llmBatchConcurrencyLimit(len(batches)))

	var wg sync.WaitGroup
	for i, batch := range batches {
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, batch []WorkItem) {
			defer wg.Done()
			defer func() { <-sem }()
			// Select relevant examples for this batch via TF-IDF.
			var batchExamples []historicalItem
			if tfidfIdx != nil {
				var queries []string
				for _, item := range batch {
					queries = append(queries, item.Description)
				}
				exampleCount := cfg.LLMExampleCount
				if exampleCount < 1 {
					exampleCount = 20
				}
				batchExamples = tfidfIdx.topKForBatch(queries, exampleCount)
			}
			systemPrompt, userPrompt := buildSectionPrompts(cfg, options, batch, existing, templateGuidance, corrections, batchExamples)

			var responseText string
			var usage LLMUsage
			var callErr error

			switch cfg.LLMProvider {
			case "openai":
				model := cfg.LLMModel
				if model == "" {
					model = defaultOpenAIModel
				}
				log.Printf("llm section-classify provider=openai model=%s items=%d sections=%d batch=%d", model, len(batch), len(options), idx)
				responseText, usage, callErr = callOpenAISectionStructured(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, systemPrompt, userPrompt, options)
			default:
				model := cfg.LLMModel
				if model == "" {
					model = defaultAnthropicModel
				}
				log.Printf("llm section-classify provider=anthropic model=%s items=%d sections=%d batch=%d", model, len(batch), len(options), idx)
				responseText, usage, callErr = callAnthropic(cfg.AnthropicAPIKey, model, systemPrompt, userPrompt)
			}

			if callErr != nil {
				results[idx] = batchResult{usage: usage, err: callErr}
				return
			}

			parsed, parseErr := parseSectionClassifiedResponse(responseText)
			if parseErr != nil {
				results[idx] = batchResult{usage: usage, err: parseErr}
				return
			}
			glossaryOverrides := applyGlossaryOverrides(batch, parsed, glossary, glossarySectionMap)
			assignLocalConfidence(parsed, options, glossaryOverrides)
			results[idx] = batchResult{decisions: parsed, usage: usage}
		}(i, batch)
	}
	wg.Wait()

	all := make(map[int64]LLMSectionDecision, len(items))
	totalUsage := LLMUsage{}
	for _, r := range results {
		totalUsage.Add(r.usage)
		if r.err != nil {
			return nil, totalUsage, r.err
		}
		for id, decision := range r.decisions {
			all[id] = decision
		}
	}

	// Generator-Critic loop: second LLM pass to catch misclassifications.
	if cfg.LLMCriticEnabled && len(all) > 0 {
		flagged, criticUsage, err := runCriticPass(cfg, items, all, options)
		totalUsage.Add(criticUsage)
		if err != nil {
			log.Printf("llm critic error (non-fatal): %v", err)
		} else {
			validSections := make(map[string]bool, len(options))
			for _, opt := range options {
				validSections[opt.ID] = true
			}
			for _, f := range flagged {
				suggested := strings.TrimSpace(f.SuggestedSectionID)
				if suggested == "" || !validSections[suggested] {
					continue
				}
				if dec, ok := all[f.ID]; ok {
					log.Printf("llm critic reclassified item=%d from=%s to=%s reason=%q", f.ID, dec.SectionID, suggested, f.Reason)
					dec.SectionID = suggested
					all[f.ID] = dec
				}
			}
		}
	}

	return all, totalUsage, nil
}

func buildSectionPrompts(cfg Config, options []sectionOption, items []WorkItem, existing []existingItemContext, templateGuidance string, corrections []ClassificationCorrection, examples []historicalItem) (string, string) {
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
	exampleMaxLen := cfg.LLMExampleMaxLen
	if len(examples) > 0 {
		// Use TF-IDF selected examples.
		var exBuf strings.Builder
		for _, ex := range examples {
			desc := strings.TrimSpace(ex.Description)
			if len(desc) > exampleMaxLen {
				desc = desc[:exampleMaxLen] + "..."
			}
			exBuf.WriteString(fmt.Sprintf("- EX|%s|%s\n", ex.SectionID, desc))
		}
		if exBuf.Len() > 0 {
			examplesBlock = exBuf.String()
		}
	} else if len(existing) > 0 {
		// Fallback: use first N existing items.
		exampleCount := cfg.LLMExampleCount
		var exBuf strings.Builder
		for i, ex := range existing {
			if i >= exampleCount {
				break
			}
			desc := strings.TrimSpace(ex.Description)
			if len(desc) > exampleMaxLen {
				desc = desc[:exampleMaxLen] + "..."
			}
			exBuf.WriteString(fmt.Sprintf("- EX|%s|%s\n", ex.SectionID, desc))
		}
		if exBuf.Len() > 0 {
			examplesBlock = exBuf.String()
		}
	}

	templateBlock := ""
	if strings.TrimSpace(templateGuidance) != "" {
		templateBlock = "\nTemplate guidance (semantic hints only; still choose section_id only from the list above):\n" + templateGuidance + "\n"
	}

	correctionsNote := ""
	if len(corrections) > 0 {
		correctionsNote = "\nA 'Past corrections' section shows previous misclassifications. Avoid repeating them."
	}

	systemPrompt := fmt.Sprintf(`You classify software work items into one section.
Choose exactly one section_id for each item from:
%s

If none fit, use section_id "UND".
Also:
- choose normalized_status from: done, in testing, in progress, other
- extract ticket IDs if present (e.g. [1247202] or bare ticket numbers); return them as an array of strings
- if this item is the same underlying work as an existing item, set duplicate_of to that existing key (Kxx); otherwise empty string
%s%s

Respond with JSON only (no markdown):
[{"id": 1, "section_id": "S0_2", "normalized_status": "in progress", "ticket_ids": ["1247202"], "duplicate_of": "K3"}, ...]`, sectionLines.String(), templateBlock, correctionsNote)

	correctionsBlock := ""
	if len(corrections) > 0 {
		var cb strings.Builder
		cb.WriteString("\nPast corrections (learn from these — avoid repeating these mistakes):\n")
		limit := 20
		if len(corrections) < limit {
			limit = len(corrections)
		}
		for i := 0; i < limit; i++ {
			c := corrections[i]
			desc := strings.TrimSpace(c.Description)
			if len(desc) > 120 {
				desc = desc[:120] + "..."
			}
			origLabel := c.OriginalSectionID
			if c.OriginalLabel != "" {
				origLabel = fmt.Sprintf("%s (%s)", c.OriginalSectionID, c.OriginalLabel)
			}
			corrLabel := c.CorrectedSectionID
			if c.CorrectedLabel != "" {
				corrLabel = fmt.Sprintf("%s (%s)", c.CorrectedSectionID, c.CorrectedLabel)
			}
			cb.WriteString(fmt.Sprintf("- \"%s\" was classified as %s, corrected to %s\n", desc, origLabel, corrLabel))
		}
		correctionsBlock = cb.String()
	}

	userPrompt := "Examples from previous reports:\n" + examplesBlock +
		"\nExisting items (for duplicate_of):\n" + existingBlock +
		correctionsBlock +
		"\nClassify these items:\n\n" + itemLines.String()
	return systemPrompt, userPrompt
}

func loadTemplateGuidance(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Optional file: no hard failure if missing.
		log.Printf("llm template guidance skipped path=%s err=%v", path, err)
		return ""
	}
	text := strings.TrimSpace(string(data))
	if len(text) > maxTemplateGuidanceChars {
		text = text[:maxTemplateGuidanceChars] + "\n...(truncated)"
	}
	return text
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
		ticketIDs := parseTicketIDsField(c.TicketIDs)
		decisions[c.ID] = LLMSectionDecision{
			SectionID:        strings.TrimSpace(c.SectionID),
			NormalizedStatus: normalizeStatus(strings.TrimSpace(c.NormalizedStatus)),
			TicketIDs:        ticketIDs,
			DuplicateOf:      strings.TrimSpace(c.DuplicateOf),
		}
	}
	return decisions, nil
}

func parseTicketIDsField(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Primary expected shape: "12345" or "12345,67890"
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}

	// Also accept model outputs like ["12345"] or [].
	var asStringSlice []string
	if err := json.Unmarshal(raw, &asStringSlice); err == nil {
		var out []string
		for _, s := range asStringSlice {
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
		}
		return strings.Join(out, ",")
	}

	// Fallback for mixed arrays.
	var asAnySlice []any
	if err := json.Unmarshal(raw, &asAnySlice); err == nil {
		var out []string
		for _, v := range asAnySlice {
			switch x := v.(type) {
			case string:
				x = strings.TrimSpace(x)
				if x != "" {
					out = append(out, x)
				}
			case float64:
				out = append(out, fmt.Sprintf("%.0f", x))
			}
		}
		return strings.Join(out, ",")
	}

	return ""
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
) map[int64]bool {
	overrides := map[int64]bool{}
	if glossary == nil {
		return overrides
	}

	for _, item := range items {
		desc := normalizeTextToken(item.Description)
		decision := decisions[item.ID]

		for phrase, sectionID := range glossarySectionMap {
			if phrase != "" && strings.Contains(desc, phrase) {
				if decision.SectionID != sectionID {
					overrides[item.ID] = true
				}
				decision.SectionID = sectionID
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
	return overrides
}

func assignLocalConfidence(decisions map[int64]LLMSectionDecision, options []sectionOption, glossaryOverrides map[int64]bool) {
	validSections := make(map[string]bool, len(options)+1)
	validSections["UND"] = true
	for _, option := range options {
		validSections[strings.TrimSpace(option.ID)] = true
	}
	for id, decision := range decisions {
		decisions[id] = withDerivedConfidence(decision, validSections, glossaryOverrides[id])
	}
}

func withDerivedConfidence(decision LLMSectionDecision, validSections map[string]bool, glossaryOverride bool) LLMSectionDecision {
	sectionID := strings.TrimSpace(decision.SectionID)
	switch {
	case glossaryOverride:
		decision.Confidence = 0.99
	case sectionID == "" || !validSections[sectionID]:
		decision.Confidence = 0.20
	case strings.EqualFold(sectionID, "UND"):
		decision.Confidence = 0.40
	case strings.TrimSpace(decision.DuplicateOf) != "":
		decision.Confidence = 0.95
	default:
		decision.Confidence = 0.90
	}
	return decision
}

// --- Anthropic ---

func callAnthropic(apiKey, model, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	message, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 4096,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt, CacheControl: anthropic.NewCacheControlEphemeralParam()},
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
		InputTokens:              message.Usage.InputTokens,
		OutputTokens:             message.Usage.OutputTokens,
		CacheCreationInputTokens: message.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     message.Usage.CacheReadInputTokens,
	}

	for _, block := range message.Content {
		if block.Type == "text" {
			log.Printf("llm anthropic response size=%d tokens_in=%d tokens_out=%d cache_create=%d cache_read=%d", len(block.Text), usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens)
			return block.Text, usage, nil
		}
	}
	return "", usage, fmt.Errorf("no text content in Anthropic response")
}

// --- OpenAI / OpenAI-compatible Responses API ---

type openAIResponsesRequest struct {
	Model       string                    `json:"model"`
	Input       string                    `json:"input"`
	Temperature float64                   `json:"temperature,omitempty"`
	Text        *openAIResponsesTextParam `json:"text,omitempty"`
}

type openAIResponsesTextParam struct {
	Format openAIResponsesFormatParam `json:"format"`
}

type openAIResponsesFormatParam struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Strict bool   `json:"strict"`
	Schema any    `json:"schema"`
}

type openAIResponsesResponse struct {
	Output []struct {
		Type    string `json:"type"`
		Role    string `json:"role,omitempty"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content,omitempty"`
	} `json:"output"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func buildSectionJSONSchema(options []sectionOption) map[string]any {
	sections := make([]string, 0, len(options)+1)
	sections = append(sections, "UND")
	for _, option := range options {
		id := strings.TrimSpace(option.ID)
		if id != "" {
			sections = append(sections, id)
		}
	}
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"id": map[string]any{
					"type": "integer",
				},
				"section_id": map[string]any{
					"type": "string",
					"enum": sections,
				},
				"normalized_status": map[string]any{
					"type": "string",
					"enum": []string{"done", "in testing", "in progress", "other"},
				},
				"ticket_ids": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
				},
				"duplicate_of": map[string]any{
					"type": "string",
				},
			},
			"required": []string{"id", "section_id", "normalized_status", "ticket_ids", "duplicate_of"},
		},
	}
}

func callOpenAISectionStructured(apiKey, baseURL, model, systemPrompt, userPrompt string, options []sectionOption) (string, LLMUsage, error) {
	reqBody := openAIResponsesRequest{
		Model: model,
		Input: buildResponsesInput(systemPrompt, userPrompt),
		Text: &openAIResponsesTextParam{
			Format: openAIResponsesFormatParam{
				Type:   "json_schema",
				Name:   "section_classification_batch",
				Strict: true,
				Schema: buildSectionJSONSchema(options),
			},
		},
	}
	responseText, usage, err := doOpenAIResponsesRequest(apiKey, baseURL, reqBody)
	if err != nil {
		return "", usage, err
	}
	log.Printf("llm openai responses size=%d tokens_in=%d tokens_out=%d", len(responseText), usage.InputTokens, usage.OutputTokens)
	return responseText, usage, nil
}

func buildResponsesInput(systemPrompt, userPrompt string) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	userPrompt = strings.TrimSpace(userPrompt)
	switch {
	case systemPrompt == "":
		return userPrompt
	case userPrompt == "":
		return systemPrompt
	default:
		return systemPrompt + "\n\n" + userPrompt
	}
}

func doOpenAIResponsesRequest(apiKey, baseURL string, reqBody openAIResponsesRequest) (string, LLMUsage, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("marshaling responses request: %w", err)
	}

	req, err := http.NewRequest("POST", strings.TrimRight(baseURL, "/")+"/responses", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("creating responses request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := externalHTTPClient.Do(req)
	if err != nil {
		log.Printf("llm openai responses error: %v", err)
		return "", LLMUsage{}, fmt.Errorf("OpenAI Responses API error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("reading responses body: %w", err)
	}

	var responsesResp openAIResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		return "", LLMUsage{}, fmt.Errorf("parsing OpenAI Responses payload: %w", err)
	}
	if responsesResp.Error != nil {
		log.Printf("llm openai responses api error: %s", responsesResp.Error.Message)
		return "", LLMUsage{}, fmt.Errorf("OpenAI Responses API error: %s", responsesResp.Error.Message)
	}

	responseText, err := extractResponsesOutputText(responsesResp)
	if err != nil {
		return "", LLMUsage{}, err
	}
	usage := LLMUsage{}
	if responsesResp.Usage != nil {
		usage.InputTokens = responsesResp.Usage.InputTokens
		usage.OutputTokens = responsesResp.Usage.OutputTokens
	}
	return responseText, usage, nil
}

func extractResponsesOutputText(resp openAIResponsesResponse) (string, error) {
	for _, output := range resp.Output {
		for _, content := range output.Content {
			if strings.TrimSpace(content.Text) == "" {
				continue
			}
			switch content.Type {
			case "output_text", "text":
				return content.Text, nil
			}
		}
	}
	return "", fmt.Errorf("no structured text content in OpenAI Responses payload")
}

func callOpenAI(apiKey, baseURL, model, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	responseText, usage, err := doOpenAIResponsesRequest(apiKey, baseURL, openAIResponsesRequest{
		Model: model,
		Input: buildResponsesInput(systemPrompt, userPrompt),
	})
	if err != nil {
		return "", usage, err
	}
	log.Printf("llm openai responses size=%d tokens_in=%d tokens_out=%d", len(responseText), usage.InputTokens, usage.OutputTokens)
	return responseText, usage, nil
}

// --- Generator-Critic Loop ---

type criticFlagged struct {
	ID                 int64  `json:"id"`
	Reason             string `json:"reason"`
	SuggestedSectionID string `json:"suggested_section_id"`
}

func runCriticPass(cfg Config, items []WorkItem, decisions map[int64]LLMSectionDecision, options []sectionOption) ([]criticFlagged, LLMUsage, error) {
	var sectionLines strings.Builder
	for _, opt := range options {
		sectionLines.WriteString(fmt.Sprintf("- %s: %s\n", opt.ID, opt.Label))
	}

	var itemLines strings.Builder
	for _, item := range items {
		dec := decisions[item.ID]
		itemLines.WriteString(fmt.Sprintf("ID:%d | section: %s | status: %s | desc: %s\n",
			item.ID, dec.SectionID, dec.NormalizedStatus, strings.TrimSpace(item.Description)))
	}

	systemPrompt := fmt.Sprintf(`You are a classification reviewer. Review the section assignments below and identify any misclassifications.

Available sections:
%s

For each misclassified item, return its ID, a brief reason, and a suggested_section_id.
Only flag items you are confident are wrong. Return an empty array [] if all assignments look correct.

Respond with JSON only (no markdown):
[{"id": 1, "reason": "...", "suggested_section_id": "S1_2"}, ...]`, sectionLines.String())

	userPrompt := "Review these classifications:\n" + itemLines.String()

	var responseText string
	var usage LLMUsage
	var err error

	switch cfg.LLMProvider {
	case "openai":
		model := cfg.LLMModel
		if model == "" {
			model = defaultOpenAIModel
		}
		log.Printf("llm critic provider=openai model=%s items=%d", model, len(items))
		responseText, usage, err = callOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, systemPrompt, userPrompt)
	default:
		model := cfg.LLMModel
		if model == "" {
			model = defaultAnthropicModel
		}
		log.Printf("llm critic provider=anthropic model=%s items=%d", model, len(items))
		responseText, usage, err = callAnthropic(cfg.AnthropicAPIKey, model, systemPrompt, userPrompt)
	}
	if err != nil {
		return nil, usage, err
	}

	flagged, parseErr := parseCriticResponse(responseText)
	return flagged, usage, parseErr
}

func parseCriticResponse(responseText string) ([]criticFlagged, error) {
	responseText = strings.TrimSpace(responseText)
	responseText = strings.TrimPrefix(responseText, "```json")
	responseText = strings.TrimPrefix(responseText, "```")
	responseText = strings.TrimSuffix(responseText, "```")
	responseText = strings.TrimSpace(responseText)

	var flagged []criticFlagged
	if err := json.Unmarshal([]byte(responseText), &flagged); err != nil {
		truncated := responseText
		if len(truncated) > 512 {
			truncated = truncated[:512] + fmt.Sprintf("... [truncated, total_length=%d]", len(responseText))
		}
		return nil, fmt.Errorf("parsing critic response: %w (truncated response: %s)", err, truncated)
	}
	return flagged, nil
}

// --- Retrospective Analysis ---

type RetroSuggestion struct {
	Title     string `json:"title"`
	Reasoning string `json:"reasoning"`
	Action    string `json:"action"` // "glossary_term" or "guide_update"
	Phrase    string `json:"phrase"`
	Section   string `json:"section"`
	GuideText string `json:"guide_text"`
}

func analyzeCorrections(cfg Config, corrections []ClassificationCorrection, options []sectionOption) ([]RetroSuggestion, LLMUsage, error) {
	if len(corrections) == 0 {
		return nil, LLMUsage{}, nil
	}

	var sectionLines strings.Builder
	for _, opt := range options {
		sectionLines.WriteString(fmt.Sprintf("- %s: %s\n", opt.ID, opt.Label))
	}

	var corrLines strings.Builder
	for _, c := range corrections {
		desc := strings.TrimSpace(c.Description)
		if len(desc) > 150 {
			desc = desc[:150] + "..."
		}
		origLabel := c.OriginalSectionID
		if c.OriginalLabel != "" {
			origLabel = c.OriginalLabel
		}
		corrLabel := c.CorrectedSectionID
		if c.CorrectedLabel != "" {
			corrLabel = c.CorrectedLabel
		}
		corrLines.WriteString(fmt.Sprintf("- \"%s\": %s -> %s\n", desc, origLabel, corrLabel))
	}

	systemPrompt := fmt.Sprintf(`You analyze classification correction patterns to suggest improvements.

Available sections:
%s

Analyze the corrections below and find patterns (phrases or topics that were repeatedly misclassified).
Only suggest patterns that appear 2+ times. Max 5 suggestions.

For each suggestion, choose an action:
- "glossary_term": A keyword/phrase that should always map to a specific section. Provide "phrase" and "section" (use the section label, e.g. "Infrastructure", not the ID).
- "guide_update": A rule to add to the classification guide. Provide "guide_text" with the rule text. Keep guide_text under 500 characters.

IMPORTANT: In "title" and "reasoning", always use section LABELS (e.g. "Data Automation & Database"), never raw section IDs (e.g. "S7_0").

Respond with JSON only (no markdown):
[{"title": "...", "reasoning": "...", "action": "glossary_term", "phrase": "...", "section": "Infrastructure", "guide_text": ""}, ...]`, sectionLines.String())

	userPrompt := "Recent classification corrections:\n" + corrLines.String()

	var responseText string
	var usage LLMUsage
	var err error

	switch cfg.LLMProvider {
	case "openai":
		model := cfg.LLMModel
		if model == "" {
			model = defaultOpenAIModel
		}
		log.Printf("llm retrospective provider=openai model=%s corrections=%d", model, len(corrections))
		responseText, usage, err = callOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, systemPrompt, userPrompt)
	default:
		model := cfg.LLMModel
		if model == "" {
			model = defaultAnthropicModel
		}
		log.Printf("llm retrospective provider=anthropic model=%s corrections=%d", model, len(corrections))
		responseText, usage, err = callAnthropic(cfg.AnthropicAPIKey, model, systemPrompt, userPrompt)
	}
	if err != nil {
		return nil, usage, err
	}

	responseText = strings.TrimSpace(responseText)
	responseText = strings.TrimPrefix(responseText, "```json")
	responseText = strings.TrimPrefix(responseText, "```")
	responseText = strings.TrimSuffix(responseText, "```")
	responseText = strings.TrimSpace(responseText)

	var suggestions []RetroSuggestion
	if err := json.Unmarshal([]byte(responseText), &suggestions); err != nil {
		return nil, usage, fmt.Errorf("parsing retrospective response: %w (response: %s)", err, responseText)
	}

	if len(suggestions) > 5 {
		suggestions = suggestions[:5]
	}
	return suggestions, usage, nil
}
