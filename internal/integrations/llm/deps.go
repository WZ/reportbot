package llm

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/httpx"
)

type Config = config.Config
type WorkItem = domain.WorkItem
type ClassificationCorrection = domain.ClassificationCorrection

type SectionOption struct {
	ID         string
	Category   int
	Subsection int
	Label      string
}

type sectionOption = SectionOption

var externalHTTPClient = httpx.ExternalHTTPClient()
