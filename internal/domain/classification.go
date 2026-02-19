package domain

import "time"

type ClassificationRecord struct {
	ID               int64
	WorkItemID       int64
	SectionID        string
	SectionLabel     string
	Confidence       float64
	NormalizedStatus string
	TicketIDs        string
	DuplicateOf      string
	LLMProvider      string
	LLMModel         string
	ClassifiedAt     time.Time
}

type ClassificationCorrection struct {
	ID                 int64
	WorkItemID         int64
	OriginalSectionID  string
	OriginalLabel      string
	CorrectedSectionID string
	CorrectedLabel     string
	Description        string
	CorrectedBy        string
	CorrectedAt        time.Time
}

type ClassificationStats struct {
	TotalClassifications int
	TotalCorrections     int
	AvgConfidence        float64
	BucketBelow50        int
	Bucket50to70         int
	Bucket70to90         int
	Bucket90Plus         int
}

type HistoricalItem struct {
	Description  string
	SectionID    string
	SectionLabel string
}
