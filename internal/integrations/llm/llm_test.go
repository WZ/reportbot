package llm

import (
	"strings"
	"testing"
)

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
