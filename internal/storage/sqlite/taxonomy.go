package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"reportbot/internal/domain"
)

func LoadTaxonomy(db *sql.DB) (domain.Taxonomy, error) {
	rows, err := db.Query(`SELECT id, parent_id, level, name, description, sort_order, retired_at IS NOT NULL FROM taxonomy_nodes ORDER BY sort_order, id`)
	if err != nil {
		return domain.Taxonomy{}, err
	}
	defer rows.Close()
	var t domain.Taxonomy
	for rows.Next() {
		var n domain.TaxonomyNode
		var parent sql.NullInt64
		if err := rows.Scan(&n.ID, &parent, &n.Level, &n.Name, &n.Description, &n.SortOrder, &n.Retired); err != nil {
			return t, err
		}
		n.ParentID = parent.Int64
		t.Nodes = append(t.Nodes, n)
	}
	return t, rows.Err()
}

func validChildLevel(parentLevel, level string) bool {
	switch parentLevel {
	case "":
		return level == domain.LevelArea
	case domain.LevelArea:
		return level == domain.LevelCategory
	case domain.LevelCategory:
		return level == domain.LevelSection
	}
	return false
}

func parentLevel(q querier, parentID int64) (string, error) {
	if parentID == 0 {
		return "", nil
	}
	var level string
	err := q.QueryRow(`SELECT level FROM taxonomy_nodes WHERE id = ?`, parentID).Scan(&level)
	return level, err
}

func insertNode(q querier, parentID int64, level, name, description string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("name is required")
	}
	pl, err := parentLevel(q, parentID)
	if err != nil {
		return 0, err
	}
	if !validChildLevel(pl, level) {
		return 0, fmt.Errorf("a %s cannot be placed under a %q node", level, pl)
	}
	var next int
	if err := q.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) + 10 FROM taxonomy_nodes WHERE COALESCE(parent_id, 0) = ?`, parentID).Scan(&next); err != nil {
		return 0, err
	}
	res, err := q.Exec(
		`INSERT INTO taxonomy_nodes (parent_id, level, name, description, sort_order, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullID(parentID), level, name, strings.TrimSpace(description), next, time.Now(), time.Now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertTaxonomyNode appends a node as the last child of parentID (0 for an Area).
func InsertTaxonomyNode(db *sql.DB, parentID int64, level, name, description string) (int64, error) {
	return insertNode(db, parentID, level, name, description)
}

func UpdateTaxonomyNode(db *sql.DB, id int64, name, description string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	_, err := db.Exec(`UPDATE taxonomy_nodes SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
		name, strings.TrimSpace(description), time.Now(), id)
	return err
}

// MoveTaxonomyNode swaps the node with its previous (delta<0) or next (delta>0) active sibling.
func MoveTaxonomyNode(db *sql.DB, id int64, delta int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var parent sql.NullInt64
	var order int
	if err := tx.QueryRow(`SELECT parent_id, sort_order FROM taxonomy_nodes WHERE id = ?`, id).Scan(&parent, &order); err != nil {
		return err
	}
	cmp, dir := ">", "ASC"
	if delta < 0 {
		cmp, dir = "<", "DESC"
	}
	var otherID int64
	var otherOrder int
	err = tx.QueryRow(
		`SELECT id, sort_order FROM taxonomy_nodes
		 WHERE COALESCE(parent_id, 0) = ? AND retired_at IS NULL AND sort_order `+cmp+` ?
		 ORDER BY sort_order `+dir+` LIMIT 1`, parent.Int64, order,
	).Scan(&otherID, &otherOrder)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET sort_order = ?, updated_at = ? WHERE id = ?`, otherOrder, time.Now(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET sort_order = ?, updated_at = ? WHERE id = ?`, order, time.Now(), otherID); err != nil {
		return err
	}
	return tx.Commit()
}

// RetireTaxonomyNode retires a node and its descendants. Their Work Items keep the
// Section until re-classification moves them.
func RetireTaxonomyNode(db *sql.DB, id int64) error {
	return retireNode(db, id)
}

func retireNode(q querier, id int64) error {
	_, err := q.Exec(
		`WITH RECURSIVE sub(id) AS (SELECT ? UNION ALL SELECT t.id FROM taxonomy_nodes t JOIN sub ON t.parent_id = sub.id)
		 UPDATE taxonomy_nodes SET retired_at = ?, updated_at = ? WHERE id IN (SELECT id FROM sub) AND retired_at IS NULL`,
		id, time.Now(), time.Now(),
	)
	return err
}

// NewSection is a Section to create by a split.
type NewSection struct {
	Name, Description string
}

// SplitSection: a Section gets new siblings and is retired; a Category-as-Section gets
// child Sections and is not retired, since retiring it would retire the new children too.
func SplitSection(db *sql.DB, id int64, parts []NewSection) error {
	if len(parts) == 0 {
		return fmt.Errorf("at least one new Section name is required")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var level string
	var parent sql.NullInt64
	if err := tx.QueryRow(`SELECT level, parent_id FROM taxonomy_nodes WHERE id = ?`, id).Scan(&level, &parent); err != nil {
		return err
	}
	under := parent.Int64
	if level == domain.LevelCategory {
		under = id
	}
	for _, p := range parts {
		if _, err := insertNode(tx, under, domain.LevelSection, p.Name, p.Description); err != nil {
			return err
		}
	}
	if level == domain.LevelSection {
		if err := retireNode(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MergeSections moves every Work Item in from into into, decided by the manager, and retires from.
func MergeSections(db *sql.DB, from, into int64, actorID string) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM work_items WHERE section_id = ?`, from)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := applyPlacement(tx, id, Placement{
			SectionID: into, PlacedBy: domain.PlacedByPerson, FilingState: domain.FilingFiled,
			HistoryNote: ReasonTaxonomyMerge, ActorID: actorID,
		}); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET retired_at = ?, updated_at = ? WHERE id = ?`, time.Now(), time.Now(), from); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit()
}

// PromoteCategoryToArea turns a Category into an Area, keeping every node ID so placed
// Work Items stay put: its Sections become the new Area's Categories. A Category that is
// itself the Section has nothing to become a Category, so it moves under a new Area of
// the same name instead.
func PromoteCategoryToArea(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var level, name, description string
	if err := tx.QueryRow(`SELECT level, name, description FROM taxonomy_nodes WHERE id = ? AND retired_at IS NULL`, id).Scan(&level, &name, &description); err != nil {
		return err
	}
	if level != domain.LevelCategory {
		return fmt.Errorf("only a Category can become an Area")
	}
	var sections int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM taxonomy_nodes WHERE parent_id = ? AND retired_at IS NULL`, id).Scan(&sections); err != nil {
		return err
	}
	if sections == 0 {
		areaID, err := insertNode(tx, 0, domain.LevelArea, name, description)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE taxonomy_nodes SET parent_id = ?, sort_order = 10, updated_at = ? WHERE id = ?`, areaID, time.Now(), id); err != nil {
			return err
		}
		return tx.Commit()
	}
	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) + 10 FROM taxonomy_nodes WHERE parent_id IS NULL`).Scan(&next); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET level = ?, parent_id = NULL, sort_order = ?, updated_at = ? WHERE id = ?`, domain.LevelArea, next, time.Now(), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE taxonomy_nodes SET level = ?, updated_at = ? WHERE parent_id = ?`, domain.LevelCategory, time.Now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// SeedTaxonomy inserts a whole tree in one transaction; it refuses when a Taxonomy exists.
func SeedTaxonomy(db *sql.DB, areas []SeedArea) (map[string]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := SeedTaxonomyTx(tx, areas)
	if err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

// SeedTaxonomyTx is SeedTaxonomy inside the caller's transaction.
func SeedTaxonomyTx(tx *sql.Tx, areas []SeedArea) (map[string]int64, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM taxonomy_nodes`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("taxonomy already seeded (%d nodes)", n)
	}
	ids := map[string]int64{}
	for _, a := range areas {
		aid, err := insertNode(tx, 0, domain.LevelArea, a.Name, "")
		if err != nil {
			return nil, err
		}
		for _, c := range a.Categories {
			cid, err := insertNode(tx, aid, domain.LevelCategory, c.Name, "")
			if err != nil {
				return nil, err
			}
			ids[SeedKey(a.Name, c.Name, "")] = cid
			for _, s := range c.Sections {
				sid, err := insertNode(tx, cid, domain.LevelSection, s, "")
				if err != nil {
					return nil, err
				}
				ids[SeedKey(a.Name, c.Name, s)] = sid
			}
		}
	}
	return ids, nil
}

type SeedArea struct {
	Name       string
	Categories []SeedCategory
}

type SeedCategory struct {
	Name     string
	Sections []string
}

// SeedKey identifies a seeded Section by its name path.
func SeedKey(area, category, section string) string {
	return area + "\x00" + category + "\x00" + section
}
