package proposals

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"reportbot/internal/domain"
	"reportbot/internal/integrations/llm"
)

// A proposed Section's text becomes decision-model criteria and Slack text, so it must be plain and inert.
var (
	urlRe          = regexp.MustCompile(`(?i)\b(?:https?|ftp)://|\bwww\.`)
	slackMentionRe = regexp.MustCompile(`<[@#!]`)
	tagRe          = regexp.MustCompile(`<[^>]*>`)
	mdLinkRe       = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)`)
	// addressesModel matches text that speaks to the model instead of describing work.
	addressesModel = []string{"ignore", "always choose", "you must", "answer", "system", "instruction"}
	// sectionRefRe finds a Section named as where work goes instead, e.g. "those are Infrastructure".
	sectionRefRe = regexp.MustCompile(`(?i:those are|that is|these are|goes? (?:to|in|under)|belongs? (?:to|in|under)|filed? (?:to|in|under)|see)\s+(?i:the\s+)?([A-Z0-9][\w./&+-]*(?:\s+[A-Z0-9][\w./&+-]*)*)`)
)

// Structure Proposal limits.
const (
	maxProposalName        = 60
	maxProposalDescription = 300
	maxProposalReason      = 500
)

// plainText rejects text that is too long or could carry links, mentions, markup or control characters.
func plainText(n string, max int) error {
	switch {
	case len([]rune(n)) > max:
		return fmt.Errorf("longer than %d characters", max)
	case urlRe.MatchString(n):
		return fmt.Errorf("contains a URL")
	case slackMentionRe.MatchString(n):
		return fmt.Errorf("contains a Slack mention")
	case strings.Contains(n, "`"):
		return fmt.Errorf("contains code formatting")
	case mdLinkRe.MatchString(n):
		return fmt.Errorf("contains a markdown link")
	case tagRe.MatchString(n):
		return fmt.Errorf("contains markup")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func namesAuthor(n string, authors []string) bool {
	for _, a := range authors {
		for _, tok := range strings.FieldsFunc(a, func(r rune) bool { return !unicode.IsLetter(r) && r != '-' && r != '\'' }) {
			if len([]rune(tok)) >= 3 && regexp.MustCompile(`\b`+regexp.QuoteMeta(tok)+`\b`).MatchString(n) {
				return true
			}
		}
	}
	return false
}

// validDescription rejects a proposed description unless it is plain text within the cap, does
// not address the model, names only existing Sections (knownNames) and names none of authors.
func validDescription(text string, knownNames, authors []string) error {
	n := strings.TrimSpace(text)
	if n == "" {
		return fmt.Errorf("empty")
	}
	if err := plainText(n, maxProposalDescription); err != nil {
		return err
	}
	lower := strings.ToLower(n)
	for _, p := range addressesModel {
		if strings.Contains(lower, p) {
			return fmt.Errorf("addresses the model (%q)", p)
		}
	}
	for _, m := range sectionRefRe.FindAllStringSubmatch(n, -1) {
		if !startsWithKnown(m[1], knownNames) {
			return fmt.Errorf("names a Section that does not exist (%q)", m[1])
		}
	}
	if namesAuthor(n, authors) {
		return fmt.Errorf("names a person")
	}
	return nil
}

// ValidateProposal checks a Structure Proposal before it is stored or shown: plain short names,
// descriptions held to validDescription, a plain reason, and an existing parent for a new Section.
func ValidateProposal(p llm.StructureProposal, knownNames []string, parents map[int64]bool, authors []string) error {
	name := func(n string) error {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("empty name")
		}
		if err := plainText(n, maxProposalName); err != nil {
			return fmt.Errorf("name %w", err)
		}
		if namesAuthor(n, authors) {
			return fmt.Errorf("name names a person")
		}
		return nil
	}
	desc := func(d string, extra []string) error {
		if strings.TrimSpace(d) == "" {
			return nil
		}
		if err := validDescription(d, append(append([]string{}, knownNames...), extra...), authors); err != nil {
			return fmt.Errorf("description %w", err)
		}
		return nil
	}
	switch p.Kind {
	case llm.ProposeNew:
		if !parents[p.ParentID] {
			return fmt.Errorf("the parent does not exist")
		}
		if err := name(p.Name); err != nil {
			return err
		}
		if err := desc(p.Description, []string{p.Name}); err != nil {
			return err
		}
	case llm.ProposeSplit:
		var names []string
		for _, part := range p.NewSections {
			names = append(names, part.Name)
		}
		for _, part := range p.NewSections {
			if err := name(part.Name); err != nil {
				return err
			}
			if err := desc(part.Description, names); err != nil {
				return err
			}
		}
	}
	if r := strings.TrimSpace(p.Reason); r != "" {
		if err := plainText(r, maxProposalReason); err != nil {
			return fmt.Errorf("reason %w", err)
		}
		if namesAuthor(r, authors) {
			return fmt.Errorf("reason names a person")
		}
	}
	return nil
}

// startsWithKnown reports whether ref begins with an existing Taxonomy node name.
func startsWithKnown(ref string, known []string) bool {
	ref = strings.ToLower(strings.TrimRight(ref, ".,;:"))
	for _, k := range known {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && (ref == k || strings.HasPrefix(ref, k+" ")) {
			return true
		}
	}
	return false
}

// TaxonomyNames are the names a proposal's text may use to refer to a Section.
func TaxonomyNames(sections []domain.Section) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sections {
		for _, n := range []string{s.Area, s.Category, s.Name} {
			if n != "" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
