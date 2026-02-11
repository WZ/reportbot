package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type LLMGlossary struct {
	Terms       []GlossaryTerm       `yaml:"terms"`
	StatusHints []GlossaryStatusHint `yaml:"status_hints"`
}

type GlossaryTerm struct {
	Phrase  string `yaml:"phrase"`
	Section string `yaml:"section"`
}

type GlossaryStatusHint struct {
	Phrase string `yaml:"phrase"`
	Status string `yaml:"status"`
}

func LoadLLMGlossary(path string) (*LLMGlossary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read glossary: %w", err)
	}
	var g LLMGlossary
	if err := yaml.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parse glossary yaml: %w", err)
	}
	return &g, nil
}

func normalizeTextToken(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func AppendGlossaryTerm(path, phrase, section string) error {
	phrase = strings.TrimSpace(phrase)
	section = strings.TrimSpace(section)
	if phrase == "" || section == "" {
		return nil
	}

	var glossary LLMGlossary
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, &glossary); err != nil {
			return fmt.Errorf("parse existing glossary: %w", err)
		}
	}

	normalized := normalizeTextToken(phrase)
	for _, t := range glossary.Terms {
		if normalizeTextToken(t.Phrase) == normalized {
			return nil // already exists
		}
	}

	glossary.Terms = append(glossary.Terms, GlossaryTerm{
		Phrase:  phrase,
		Section: section,
	})
	return saveGlossary(path, &glossary)
}

func saveGlossary(path string, glossary *LLMGlossary) error {
	data, err := yaml.Marshal(glossary)
	if err != nil {
		return fmt.Errorf("marshal glossary: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

func extractGlossaryPhrase(description string) string {
	s := strings.TrimSpace(description)
	// Strip leading ticket prefixes like [12345]
	for strings.HasPrefix(s, "[") {
		idx := strings.Index(s, "]")
		if idx < 0 {
			break
		}
		s = strings.TrimSpace(s[idx+1:])
	}
	s = strings.ToLower(s)
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
