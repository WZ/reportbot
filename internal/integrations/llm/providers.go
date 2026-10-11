package llm

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

// ModelName is the configured model, or the provider default.
func ModelName(cfg Config) string {
	if cfg.LLMModel != "" {
		return cfg.LLMModel
	}
	if cfg.LLMProvider == "openai" {
		return defaultOpenAIModel
	}
	return defaultAnthropicModel
}

// --- Anthropic ---

func callAnthropic(ctx context.Context, apiKey, model, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	client := anthropic.NewClient(option.WithAPIKey(apiKey))

	message, err := client.Messages.New(ctx, anthropic.MessageNewParams{
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
	Reasoning   *openAIReasoningParam     `json:"reasoning,omitempty"`
}

type openAIReasoningParam struct {
	Effort string `json:"effort"`
}

// reasoningParam is nil when no effort is configured, so the provider default applies.
func reasoningParam(effort string) *openAIReasoningParam {
	if effort == "" {
		return nil
	}
	return &openAIReasoningParam{Effort: effort}
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

func doOpenAIResponsesRequest(ctx context.Context, apiKey, baseURL string, reqBody openAIResponsesRequest) (string, LLMUsage, error) {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", LLMUsage{}, fmt.Errorf("marshaling responses request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/responses", bytes.NewReader(bodyBytes))
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

	if resp.StatusCode != http.StatusOK {
		truncated := string(respBody)
		if len(truncated) > 512 {
			truncated = truncated[:512] + "..."
		}
		log.Printf("llm openai responses HTTP %d: %s", resp.StatusCode, truncated)
		return "", LLMUsage{}, fmt.Errorf("OpenAI Responses API HTTP %d: %s", resp.StatusCode, truncated)
	}

	var responsesResp openAIResponsesResponse
	if err := json.Unmarshal(respBody, &responsesResp); err != nil {
		truncated := string(respBody)
		if len(truncated) > 512 {
			truncated = truncated[:512] + "..."
		}
		log.Printf("llm openai responses invalid JSON: %s", truncated)
		return "", LLMUsage{}, fmt.Errorf("parsing OpenAI Responses payload: %w (body: %s)", err, truncated)
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
	// Reasoning models return a "reasoning" output followed by a "message"
	// output. Prefer the "message" output; fall back to any output_text.
	var fallback string
	for _, output := range resp.Output {
		isReasoning := output.Type == "reasoning"
		for _, content := range output.Content {
			if strings.TrimSpace(content.Text) == "" {
				continue
			}
			switch content.Type {
			case "output_text", "text":
				if !isReasoning {
					return content.Text, nil
				}
				if fallback == "" {
					fallback = content.Text
				}
			}
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no structured text content in OpenAI Responses payload")
}

func callOpenAI(ctx context.Context, apiKey, baseURL, model, effort, systemPrompt, userPrompt string) (string, LLMUsage, error) {
	responseText, usage, err := doOpenAIResponsesRequest(ctx, apiKey, baseURL, openAIResponsesRequest{
		Model:     model,
		Input:     buildResponsesInput(systemPrompt, userPrompt),
		Reasoning: reasoningParam(effort),
	})
	if err != nil {
		return "", usage, err
	}
	log.Printf("llm openai responses size=%d tokens_in=%d tokens_out=%d", len(responseText), usage.InputTokens, usage.OutputTokens)
	return responseText, usage, nil
}

// Complete sends one system+user prompt to the configured LLM and returns its text.
func Complete(ctx context.Context, cfg Config, system, user string) (string, LLMUsage, error) {
	model := ModelName(cfg)
	if cfg.LLMProvider == "openai" {
		return callOpenAI(ctx, cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, model, cfg.LLMReasoningEffort, system, user)
	}
	return callAnthropic(ctx, cfg.AnthropicAPIKey, model, system, user)
}
