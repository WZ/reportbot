package web

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
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
