package domain

import (
	"sort"
	"strings"
)

const (
	LevelArea     = "area"
	LevelCategory = "category"
	LevelSection  = "section"
)

type TaxonomyNode struct {
	ID          int64
	ParentID    int64 // 0 for an Area
	Level       string
	Name        string
	Description string
	SortOrder   int
	Retired     bool
}

// Section is a fileable node: a section node, or a category node without active sections.
type Section struct {
	ID          int64
	Area        string
	Category    string
	Name        string // empty when the Category is itself the Section
	Description string
	CategoryID  int64
	AreaID      int64
}

const PathSeparator = " › "

// Path is "Area › Category › Section", omitting empty parts.
func (s Section) Path() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{s.Area, s.Category, s.Name} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, PathSeparator)
}

// Label is the deepest name: the Section, or the Category when it is the Section.
func (s Section) Label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Category
}

// Taxonomy is the ordered tree; Nodes may include retired ones.
type Taxonomy struct {
	Nodes []TaxonomyNode
}

func (t Taxonomy) children(parentID int64, includeRetired bool) []TaxonomyNode {
	var out []TaxonomyNode
	for _, n := range t.Nodes {
		if n.ParentID == parentID && (includeRetired || !n.Retired) {
			out = append(out, n)
		}
	}
	sortNodes(out)
	return out
}

func sortNodes(nodes []TaxonomyNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].SortOrder != nodes[j].SortOrder {
			return nodes[i].SortOrder < nodes[j].SortOrder
		}
		return nodes[i].ID < nodes[j].ID
	})
}

func (t Taxonomy) Areas() []TaxonomyNode { return t.children(0, false) }

func (t Taxonomy) Children(id int64) []TaxonomyNode { return t.children(id, false) }

func (t Taxonomy) Node(id int64) (TaxonomyNode, bool) {
	for _, n := range t.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return TaxonomyNode{}, false
}

// Sections lists the active fileable Sections in report order.
func (t Taxonomy) Sections() []Section {
	var out []Section
	for _, area := range t.Areas() {
		for _, cat := range t.Children(area.ID) {
			if cat.Level != LevelCategory {
				continue
			}
			secs := t.Children(cat.ID)
			if len(secs) == 0 {
				out = append(out, Section{ID: cat.ID, Area: area.Name, Category: cat.Name, Description: cat.Description, CategoryID: cat.ID, AreaID: area.ID})
				continue
			}
			for _, sec := range secs {
				out = append(out, Section{ID: sec.ID, Area: area.Name, Category: cat.Name, Name: sec.Name, Description: sec.Description, CategoryID: cat.ID, AreaID: area.ID})
			}
		}
	}
	return out
}

// Section returns the fileable Section with this id, if it is active.
func (t Taxonomy) Section(id int64) (Section, bool) {
	for _, s := range t.Sections() {
		if s.ID == id {
			return s, true
		}
	}
	return Section{}, false
}

// SectionAny resolves a node id to its Section view even when retired, for history display.
func (t Taxonomy) SectionAny(id int64) (Section, bool) {
	n, ok := t.Node(id)
	if !ok || n.Level == LevelArea {
		return Section{}, false
	}
	s := Section{ID: n.ID, Description: n.Description}
	cat := n
	if n.Level == LevelSection {
		s.Name = n.Name
		if c, ok := t.Node(n.ParentID); ok {
			cat = c
		}
	}
	s.Category, s.CategoryID = cat.Name, cat.ID
	if a, ok := t.Node(cat.ParentID); ok {
		s.Area, s.AreaID = a.Name, a.ID
	}
	return s, true
}
