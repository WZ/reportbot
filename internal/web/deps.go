package web

import (
	"context"
	"time"

	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/learning"
	"reportbot/internal/publish"
	"reportbot/internal/storage/sqlite"
)

type Config = config.Config
type WorkItem = domain.WorkItem

// Package-level function vars so tests can replace a dependency.
var (
	GetWorkItemByID      = sqlite.GetWorkItemByID
	UpdateWorkItemFields = sqlite.UpdateWorkItemFields
	SetFilingPending     = sqlite.SetFilingPending
	SetPlacement         = sqlite.SetPlacement
	DeleteWorkItemByID   = sqlite.DeleteWorkItemByID
	ReportVersion        = sqlite.ReportVersion

	LoadTaxonomy       = sqlite.LoadTaxonomy
	InsertTaxonomyNode = sqlite.InsertTaxonomyNode
	UpdateTaxonomyNode = sqlite.UpdateTaxonomyNode
	MoveTaxonomyNode   = sqlite.MoveTaxonomyNode
	RetireTaxonomyNode = sqlite.RetireTaxonomyNode
	MergeSections      = sqlite.MergeSections
	SplitSection       = sqlite.SplitSection
	SetLearnedNotes    = sqlite.SetLearnedNotes
	EnqueueReclassify  = sqlite.EnqueueReclassify
	PendingReclassify  = sqlite.PendingReclassify
	GetPublishedReport = sqlite.GetPublishedReport
	ListPublishedWeeks = sqlite.ListPublishedWeeks

	LoadRange = publish.LoadRange
	Publish   = publish.Publish

	ReportWeekRange = domain.ReportWeekRange
)

var IsManagerID = func(cfg Config, userID string) bool {
	return cfg.IsManagerID(userID)
}

// Loops is what the web UI uses of the learning loops (*learning.Learner); nil without an LLM.
type Loops interface {
	SweepUndetermined(ctx context.Context, items []domain.WorkItem, timeout time.Duration) int
	ProposeStructure(ctx context.Context) ([]learning.StoredProposal, error)
}
