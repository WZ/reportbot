package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func GenerateReport(items []WorkItem, reportDate time.Time, mode string, categories []string, teamName string) string {
	if mode == "" {
		mode = "team"
	}

	sections := groupByCategory(items)

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("### %s %s\n\n", teamName, reportDate.Format("20060102")))

	for _, cat := range categories {
		section, ok := sections[cat]
		if !ok || len(section) == 0 {
			continue
		}

		authors := uniqueAuthors(section)

		if mode == "boss" {
			authorStr := ""
			if len(authors) > 0 {
				authorStr = " (" + strings.Join(authors, ", ") + ")"
			}
			buf.WriteString(fmt.Sprintf("#### %s%s\n\n", cat, authorStr))
		} else {
			buf.WriteString(fmt.Sprintf("#### %s\n\n", cat))
		}

		for _, item := range section {
			prefix := ""
			if item.TicketIDs != "" {
				prefix = fmt.Sprintf("[%s] ", item.TicketIDs)
			}

			status := item.Status
			if status == "" {
				status = "done"
			}

			if mode == "team" {
				buf.WriteString(fmt.Sprintf("- **%s** - %s%s (%s)\n", item.Author, prefix, item.Description, status))
			} else {
				buf.WriteString(fmt.Sprintf("- %s%s (%s)\n", prefix, item.Description, status))
			}
		}
		buf.WriteString("\n")
	}

	// Handle uncategorized items
	var uncategorized []WorkItem
	for _, item := range items {
		found := false
		for _, cat := range categories {
			if item.Category == cat {
				found = true
				break
			}
		}
		if !found {
			uncategorized = append(uncategorized, item)
		}
	}
	if len(uncategorized) > 0 {
		buf.WriteString("#### Uncategorized\n\n")
		for _, item := range uncategorized {
			status := item.Status
			if status == "" {
				status = "done"
			}
			if mode == "team" {
				buf.WriteString(fmt.Sprintf("- **%s** - %s (%s)\n", item.Author, item.Description, status))
			} else {
				buf.WriteString(fmt.Sprintf("- %s (%s)\n", item.Description, status))
			}
		}
		buf.WriteString("\n")
	}

	return buf.String()
}

func WriteReportFile(content, outputDir string, reportDate time.Time, teamName string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("%s_%s.md", teamName, reportDate.Format("20060102"))
	path := filepath.Join(outputDir, filename)
	return path, os.WriteFile(path, []byte(content), 0644)
}

func groupByCategory(items []WorkItem) map[string][]WorkItem {
	sections := make(map[string][]WorkItem)
	for _, item := range items {
		cat := item.Category
		if cat == "" {
			cat = "Uncategorized"
		}
		sections[cat] = append(sections[cat], item)
	}
	return sections
}

func uniqueAuthors(items []WorkItem) []string {
	seen := make(map[string]bool)
	var authors []string
	for _, item := range items {
		if !seen[item.Author] {
			seen[item.Author] = true
			authors = append(authors, item.Author)
		}
	}
	return authors
}
