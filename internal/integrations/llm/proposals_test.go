package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeResponses serves the OpenAI Responses API with a fixed text.
func fakeResponses(t *testing.T, text string, body *map[string]any) Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if body != nil {
			_ = json.Unmarshal(b, body)
		}
		out, _ := json.Marshal(map[string]any{
			"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
			"usage":  map[string]int{"input_tokens": 100, "output_tokens": 9},
		})
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return Config{LLMProvider: "openai", OpenAIAPIKey: "k", OpenAIBaseURL: srv.URL, LLMModel: "m"}
}

func TestCompleteSendsReasoningEffortOnlyWhenSet(t *testing.T) {
	var body map[string]any
	cfg := fakeResponses(t, "ok", &body)
	text, usage, err := Complete(context.Background(), cfg, "sys", "user")
	if err != nil || text != "ok" || usage.InputTokens != 100 {
		t.Fatalf("complete: %q %+v %v", text, usage, err)
	}
	if _, sent := body["reasoning"]; sent {
		t.Fatalf("no reasoning effort configured, none sent: %v", body["reasoning"])
	}
	cfg.LLMReasoningEffort = "low"
	if _, _, err := Complete(context.Background(), cfg, "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if r, _ := body["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("expected reasoning effort low, got %v", body["reasoning"])
	}
}

func TestCompleteHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := Complete(ctx, Config{LLMProvider: "openai", OpenAIBaseURL: srv.URL}, "s", "u"); err == nil {
		t.Fatal("expected an error when the context ends")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the call must stop when the context ends")
	}
}

func TestParseProposalsValidatesEachKind(t *testing.T) {
	in := ProposalInput{
		Parents:   []ProposalParent{{ID: 3, Path: "Product A › Engineering", Level: "category"}},
		Unplaced:  []ProposalItem{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}, {ID: 6}},
		Large:     []ProposalSectionItems{{SectionID: 14, Path: "Product A › Engineering › Cluster Manager", Items: []ProposalItem{{ID: 10}, {ID: 11}}}},
		Retirable: []ProposalSection{{ID: 30, Path: "Product A › Old"}},
	}
	text := `[
	 {"kind":"new","parent_id":3,"name":"Telemetry","description":"Metrics export","item_ids":[1,2,3,4,5],"reason":"r1"},
	 {"kind":"new","parent_id":3,"name":"Too few","description":"d","item_ids":[1,2],"reason":"r"},
	 {"kind":"split","section_id":14,"new_sections":[{"name":"Join","description":"d","item_ids":[10]},{"name":"Evict","description":"d","item_ids":[11]}],"reason":"r2"},
	 {"kind":"retire","section_id":30,"reason":"r3"},
	 {"kind":"retire","section_id":14,"reason":"still used"}
	]`
	got, err := ParseProposals(text, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Kind != "new" || got[1].Kind != "split" || got[2].Kind != "retire" {
		t.Fatalf("want one valid proposal of each kind: %+v", got)
	}
	if got[0].ParentID != 3 || got[0].Name != "Telemetry" || len(got[0].ItemIDs) != 5 {
		t.Fatalf("new Section: %+v", got[0])
	}
	if got[1].SectionID != 14 || len(got[1].NewSections) != 2 {
		t.Fatalf("split: %+v", got[1])
	}
	if got[2].SectionID != 30 {
		t.Fatalf("retire: %+v", got[2])
	}
	many := `[{"kind":"retire","section_id":30},{"kind":"retire","section_id":30},{"kind":"retire","section_id":30},{"kind":"retire","section_id":30}]`
	if got, _ := ParseProposals(many, in); len(got) != 1 {
		t.Fatalf("duplicates are dropped: %+v", got)
	}
	if got, err := ParseProposals("[]", in); err != nil || len(got) != 0 {
		t.Fatalf("zero proposals is a valid answer: %v %v", got, err)
	}
}

func TestProposalPromptListsEvidence(t *testing.T) {
	in := ProposalInput{
		Sections:  []ProposalSection{{ID: 14, Path: "Product A › Engineering › Cluster Manager", Description: "d", Recent: 9}},
		Parents:   []ProposalParent{{ID: 3, Path: "Product A › Engineering", Level: "category"}},
		Unplaced:  []ProposalItem{{ID: 1, Description: "Export metrics"}},
		Retirable: []ProposalSection{{ID: 30, Path: "Product A › Old"}},
	}
	system, user := BuildProposalPrompt(in)
	for _, want := range []string{"0 to 3", "at least 5", "S14: Product A › Engineering › Cluster Manager", "9 Work Items",
		"P3: Product A › Engineering", "#1: Export metrics", "S30: Product A › Old"} {
		if !strings.Contains(system+user, want) {
			t.Fatalf("prompt lacks %q:\n%s\n%s", want, system, user)
		}
	}
}
