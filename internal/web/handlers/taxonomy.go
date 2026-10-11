package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"reportbot/internal/domain"
	"reportbot/internal/storage/sqlite"
	"reportbot/internal/web"
	"reportbot/internal/web/middleware"
	"reportbot/internal/web/templates"

	"github.com/go-chi/chi/v5"
)

// TaxonomyPage shows the active Taxonomy tree with its edit forms.
func TaxonomyPage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tax, err := web.LoadTaxonomy(db)
		if err != nil {
			log.Printf("Error loading taxonomy: %v", err)
			http.Error(w, "Failed to load Taxonomy", http.StatusInternalServerError)
			return
		}
		pending, err := web.PendingReclassify(db)
		if err != nil {
			log.Printf("Error checking re-classification: %v", err)
		}
		data := templates.TaxonomyPageData{
			CSRFToken:     web.GetCSRFToken(r),
			Reclassifying: pending,
			SectionGroups: sectionGroups(tax),
		}
		sections := map[int64]bool{}
		for _, s := range tax.Sections() {
			sections[s.ID] = true
		}
		var build func(n domain.TaxonomyNode) templates.TaxNodeData
		build = func(n domain.TaxonomyNode) templates.TaxNodeData {
			d := templates.TaxNodeData{ID: n.ID, Level: n.Level, Name: n.Name, Description: n.Description,
				LearnedNotes: n.LearnedNotes, IsSection: sections[n.ID]}
			for _, c := range tax.Children(n.ID) {
				d.Children = append(d.Children, build(c))
			}
			return d
		}
		for _, a := range tax.Areas() {
			data.Areas = append(data.Areas, build(a))
		}
		if err := templates.TaxonomyPage(data).Render(r.Context(), w); err != nil {
			log.Printf("Error rendering taxonomy page: %v", err)
		}
	}
}

// taxonomyEdit wraps a Taxonomy change: on success it queues re-classification of
// Open Work Items, wakes the filing worker and returns to the Taxonomy page.
func taxonomyEdit(db *sql.DB, wake func(), edit func(r *http.Request, tax domain.Taxonomy) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tax, err := web.LoadTaxonomy(db)
		if err != nil {
			log.Printf("Error loading taxonomy: %v", err)
			http.Error(w, "Failed to load Taxonomy", http.StatusInternalServerError)
			return
		}
		if err := edit(r, tax); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := web.EnqueueReclassify(db); err != nil {
			log.Printf("Error queuing re-classification: %v", err)
			http.Error(w, "Saved, but failed to queue re-filing of open Work Items", http.StatusInternalServerError)
			return
		}
		wake()
		http.Redirect(w, r, "/taxonomy", http.StatusSeeOther)
	}
}

// pathNode resolves {id} to an active node.
func pathNode(r *http.Request, tax domain.Taxonomy) (domain.TaxonomyNode, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return domain.TaxonomyNode{}, fmt.Errorf("invalid node id")
	}
	n, ok := tax.Node(id)
	if !ok || n.Retired {
		return n, fmt.Errorf("unknown Taxonomy node")
	}
	return n, nil
}

func AddTaxonomyNode(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, _ domain.Taxonomy) error {
		parentID, err := strconv.ParseInt(r.FormValue("parent_id"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid parent_id")
		}
		_, err = web.InsertTaxonomyNode(db, parentID, r.FormValue("level"), r.FormValue("name"), r.FormValue("description"))
		return err
	})
}

func UpdateTaxonomyNode(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		return web.UpdateTaxonomyNode(db, n.ID, r.FormValue("name"), r.FormValue("description"))
	})
}

func MoveTaxonomyNode(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		switch r.FormValue("dir") {
		case "up":
			return web.MoveTaxonomyNode(db, n.ID, -1)
		case "down":
			return web.MoveTaxonomyNode(db, n.ID, 1)
		}
		return fmt.Errorf("dir must be up or down")
	})
}

func RetireTaxonomyNode(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		return web.RetireTaxonomyNode(db, n.ID)
	})
}

// SetTaxonomyNotes edits or clears a Section's Learned Notes; the manager's description is untouched.
func SetTaxonomyNotes(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		if _, ok := tax.Section(n.ID); !ok {
			return fmt.Errorf("only a Section has learned notes")
		}
		return web.SetLearnedNotes(db, n.ID, r.FormValue("learned_notes"), middleware.UserID(r))
	})
}

// MergeTaxonomySection moves every Work Item of one Section into another and retires the first.
func MergeTaxonomySection(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		into, err := strconv.ParseInt(r.FormValue("into"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid into")
		}
		if _, ok := tax.Section(n.ID); !ok {
			return fmt.Errorf("only a Section can be merged")
		}
		if _, ok := tax.Section(into); !ok || into == n.ID {
			return fmt.Errorf("merge target must be another Section")
		}
		_, err = web.MergeSections(db, n.ID, into, middleware.UserID(r))
		return err
	})
}

// SplitTaxonomySection replaces a Section with new ones (see sqlite.SplitSection).
func SplitTaxonomySection(db *sql.DB, wake func()) http.HandlerFunc {
	return taxonomyEdit(db, wake, func(r *http.Request, tax domain.Taxonomy) error {
		n, err := pathNode(r, tax)
		if err != nil {
			return err
		}
		if _, ok := tax.Section(n.ID); !ok {
			return fmt.Errorf("only a Section can be split")
		}
		var parts []sqlite.NewSection
		for _, line := range strings.Split(r.FormValue("names"), "\n") {
			if name := strings.TrimSpace(line); name != "" {
				parts = append(parts, sqlite.NewSection{Name: name})
			}
		}
		return web.SplitSection(db, n.ID, parts)
	})
}
