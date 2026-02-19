package report

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	illm "reportbot/internal/integrations/llm"
	"time"
)

type Config = config.Config
type WorkItem = domain.WorkItem
type ClassificationCorrection = domain.ClassificationCorrection
type historicalItem = domain.HistoricalItem
type existingItemContext = illm.ExistingItemContext
type LLMSectionDecision = illm.LLMSectionDecision
type LLMUsage = illm.LLMUsage
type sectionOption = illm.SectionOption

func CategorizeItemsToSections(
	cfg Config,
	items []WorkItem,
	options []sectionOption,
	existing []existingItemContext,
	corrections []ClassificationCorrection,
	historicalItems []historicalItem,
) (map[int64]LLMSectionDecision, LLMUsage, error) {
	return illm.CategorizeItemsToSections(cfg, items, options, existing, corrections, historicalItems)
}

func FridayOfWeek(monday time.Time) time.Time {
	return domain.FridayOfWeek(monday)
}

func RenderTeamMarkdown(t *ReportTemplate) string {
	return renderTeamMarkdown(t)
}

func RenderBossMarkdown(t *ReportTemplate) string {
	return renderBossMarkdown(t)
}

func LoadTemplateForGeneration(outputDir, teamName string, reportDate time.Time) (*ReportTemplate, error) {
	t, _, err := loadTemplateForGeneration(outputDir, teamName, reportDate)
	return t, err
}

func TemplateOptions(t *ReportTemplate) []illm.SectionOption {
	return templateOptions(t)
}

func ParseTemplate(content string) *ReportTemplate {
	return parseTemplate(content)
}

func StripCurrentTeamTitleFromPrefix(t *ReportTemplate, teamName string) {
	stripCurrentTeamTitleFromPrefix(t, teamName)
}

func NormalizeStatus(status string) string {
	return normalizeStatus(status)
}

func SynthesizeName(name string) string {
	return synthesizeName(name)
}
