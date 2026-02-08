package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type classifiedItem struct {
	ID        int64  `json:"id"`
	Category  string `json:"category"`
	TicketIDs string `json:"ticket_ids"`
}

const defaultAnthropicModel = "claude-sonnet-4-5-20250929"
const defaultOpenAIModel = "gpt-4o"

func CategorizeItems(cfg Config, items []WorkItem) (map[int64]string, map[int64]string, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}

	systemPrompt, userPrompt := buildPrompts(cfg.Categories, items)

	var responseText string
	var err error

	switch cfg.LLMProvider {
	case "openai":
		model := cfg.LLMModel
		if model == "" {
			model = defaultOpenAIModel
		}
		responseText, err = callOpenAI(cfg.OpenAIAPIKey, model, systemPrompt, userPrompt)
	default:
		model := cfg.LLMModel
		if model == "" {
			model = defaultAnthropicModel
		}
		responseText, err = callAnthropic(cfg.AnthropicAPIKey, model, systemPrompt, userPrompt)
	}
	if err != nil {
		return nil, nil, err
	}

	return parseClassifiedResponse(responseText)
}

func buildPrompts(categories []string, items []WorkItem) (string, string) {
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("ID:%d - %s (by %s)\n", item.ID, item.Description, item.Author))
	}

	categoryList := strings.Join(categories, "\n- ")

	systemPrompt := fmt.Sprintf(`You are a work item classifier for a software development team.
Classify each work item into exactly one of these categories:
- %s

Also extract any ticket IDs (numbers in square brackets like [1247202] or bare numbers like 1247202 that appear to be ticket references).

Respond with JSON only. No markdown fences. Format:
[{"id": 1, "category": "Infrastructure", "ticket_ids": "1247202,1230118"}, ...]

If unsure about category, use "Uncategorized".`, categoryList)

	userPrompt := "Classify these work items:\n\n" + sb.String()
	return systemPrompt, userPrompt
}

func parseClassifiedResponse(responseText string) (map[int64]string, map[int64]string, error) {
	responseText = strings.TrimSpace(responseText)
	responseText = strings.TrimPrefix(responseText, "```json")
	responseText = strings.TrimPrefix(responseText, "```")
	responseText = strings.TrimSuffix(responseText, "```")
	responseText = strings.TrimSpace(responseText)

	var classified []classifiedItem
	if err := json.Unmarshal([]byte(responseText), &classified); err != nil {
		return nil, nil, fmt.Errorf("parsing LLM response: %w (response: %s)", err, responseText)
	}

	categoryMap := make(map[int64]string)
	ticketMap := make(map[int64]string)
	for _, c := range classified {
		if c.Category != "" {
			categoryMap[c.ID] = c.Category
		}
		if c.TicketIDs != "" {
			ticketMap[c.ID] = c.TicketIDs
		}
	}

	return categoryMap, ticketMap, nil
}

// --- Anthropic ---

func callAnthropic(apiKey, model, systemPrompt, userPrompt string) (string, error) {
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
		return "", fmt.Errorf("Anthropic API error: %w", err)
	}

	for _, block := range message.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in Anthropic response")
}

// --- OpenAI ---

type openAIRequest struct {
	Model    string           `json:"model"`
	Messages []openAIMessage  `json:"messages"`
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
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func callOpenAI(apiKey, model, systemPrompt, userPrompt string) (string, error) {
	reqBody := openAIRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenAI API error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var openAIResp openAIResponse
	if err := json.Unmarshal(respBody, &openAIResp); err != nil {
		return "", fmt.Errorf("parsing OpenAI response: %w", err)
	}

	if openAIResp.Error != nil {
		return "", fmt.Errorf("OpenAI API error: %s", openAIResp.Error.Message)
	}

	if len(openAIResp.Choices) == 0 {
		return "", fmt.Errorf("no choices in OpenAI response")
	}

	return openAIResp.Choices[0].Message.Content, nil
}
