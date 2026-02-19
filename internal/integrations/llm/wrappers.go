package llm

func AnalyzeCorrections(cfg Config, corrections []ClassificationCorrection, options []SectionOption) ([]RetroSuggestion, LLMUsage, error) {
	return analyzeCorrections(cfg, corrections, options)
}
