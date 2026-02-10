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
