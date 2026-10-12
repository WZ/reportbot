package filing

import (
	"database/sql"
	"log"
	"time"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
)

// FallbackWait is how long a reporter has to answer "Where does this go?" before the
// Work Item is filed under the decision model's #1 Section.
const FallbackWait = 24 * time.Hour

// eligibleForFallback: Undetermined because the decision model was unsure, and nobody has picked.
func eligibleForFallback(it domain.WorkItem) bool {
	return it.SectionID == 0 && it.FilingState == domain.FilingAsking && it.UndeterminedReason == domain.ReasonLowConfidence
}

// FileByFallback files each unsure, unanswered item under the most likely active Section of its
// stored Section Choice, without calling a model; atPublish tells the reporter why.
func FileByFallback(db *sql.DB, n Notifier, items []domain.WorkItem, atPublish bool) int {
	tax, err := sqlite.LoadTaxonomy(db)
	if err != nil {
		log.Printf("filing fallback: %v", err)
		return 0
	}
	filed := 0
	for _, it := range items {
		if !eligibleForFallback(it) {
			continue
		}
		answer, err := sqlite.LatestSectionChoice(db, it.ID)
		if err != nil {
			log.Printf("filing fallback item=%d: %v", it.ID, err)
			continue
		}
		var sec domain.Section
		for _, id := range sqlite.TopSections(answer, len(tax.Nodes)) {
			if s, ok := tax.Section(id); ok {
				sec = s
				break
			}
		}
		if sec.ID == 0 {
			continue
		}
		ok, err := sqlite.PlaceByFallback(db, it.ID, sec.ID)
		if err != nil {
			log.Printf("filing fallback item=%d: %v", it.ID, err)
		}
		if !ok {
			continue
		}
		filed++
		if n != nil {
			if item, err := sqlite.GetWorkItemByID(db, it.ID); err == nil {
				n.FiledByFallback(item, sec, atPublish)
			}
		}
	}
	if filed > 0 {
		log.Printf("filing fallback: filed %d Work Items under the model's first choice (publish=%t)", filed, atPublish)
	}
	return filed
}

// runDueFallbacks files the items whose reporter has not answered within FallbackWait.
func (f *Filer) runDueFallbacks() {
	items, err := sqlite.GetFallbackCandidates(f.DB, f.now().Add(-FallbackWait))
	if err != nil {
		log.Printf("filing fallback: %v", err)
		return
	}
	FileByFallback(f.DB, f.Notify, items, false)
}
