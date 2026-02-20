package gitlab

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type gitlabMRResponse struct {
	Title       string `json:"title"`
	WebURL      string `json:"web_url"`
	Description string `json:"description"`
	MergedAt    string `json:"merged_at"`
	UpdatedAt   string `json:"updated_at"`
	CreatedAt   string `json:"created_at"`
	State       string `json:"state"`
	Author      struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"author"`
	Labels     []string `json:"labels"`
	References struct {
		Full string `json:"full"`
	} `json:"references"`
}

var markdownHeadingRe = regexp.MustCompile(`^\s*#{1,6}\s+\S`)

func FetchMRs(cfg Config, from, to time.Time) ([]GitLabMR, error) {
	since := from.Format("2006-01-02T15:04:05Z")
	groupID := url.PathEscape(cfg.GitLabGroupID)
	ticketFieldLabel := strings.TrimSpace(cfg.GitLabRefTicketLabel)

	var allMRs []GitLabMR
	page := 1
	log.Printf("gitlab fetch start group=%s since=%s", cfg.GitLabGroupID, since)

	for {
		apiURL := fmt.Sprintf("%s/api/v4/groups/%s/merge_requests?state=all&updated_after=%s&per_page=100&page=%d",
			strings.TrimRight(cfg.GitLabURL, "/"), groupID, since, page)
		log.Printf("gitlab fetch page=%d", page)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("PRIVATE-TOKEN", cfg.GitLabToken)

		resp, err := externalHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching MRs: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, string(body))
		}

		var mrs []gitlabMRResponse
		if err := json.Unmarshal(body, &mrs); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		for _, mr := range mrs {
			mergedAt, err := time.Parse(time.RFC3339, mr.MergedAt)
			if err != nil {
				mergedAt = time.Time{}
			}

			updatedAt, err := time.Parse(time.RFC3339, mr.UpdatedAt)
			if err != nil {
				updatedAt = time.Time{}
			}

			createdAt, err := time.Parse(time.RFC3339, mr.CreatedAt)
			if err != nil {
				createdAt = time.Time{}
			}

			state := strings.ToLower(strings.TrimSpace(mr.State))
			switch state {
			case "merged":
				if mergedAt.Before(from) || !mergedAt.Before(to) {
					continue
				}
			case "opened":
				// For open MRs, include ones updated during the week window.
				if updatedAt.IsZero() || updatedAt.Before(from) || !updatedAt.Before(to) {
					continue
				}
			default:
				continue
			}

			projectPath := extractProjectPath(mr.WebURL)

			allMRs = append(allMRs, GitLabMR{
				Title:       mr.Title,
				Author:      mr.Author.Username,
				AuthorName:  mr.Author.Name,
				WebURL:      mr.WebURL,
				TicketIDs:   parseTicketIDsFromDescription(mr.Description, ticketFieldLabel),
				MergedAt:    mergedAt,
				UpdatedAt:   updatedAt,
				CreatedAt:   createdAt,
				State:       state,
				Labels:      mr.Labels,
				ProjectPath: projectPath,
			})
		}

		if len(mrs) < 100 {
			break
		}
		page++
	}

	log.Printf("gitlab fetch done total=%d", len(allMRs))
	return allMRs, nil
}

func parseTicketIDsFromDescription(description, fieldLabel string) string {
	description = strings.TrimSpace(description)
	fieldLabel = strings.TrimSpace(fieldLabel)
	if description == "" || fieldLabel == "" {
		return ""
	}

	fieldRe := regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:[-*]\s*)?(?:\*\*|__)?\s*` +
		regexp.QuoteMeta(fieldLabel) + `\s*(?:\*\*|__)?\s*:\s*(.*)$`)

	lines := strings.Split(description, "\n")
	raw := ""
	foundField := false
	for i, line := range lines {
		matches := fieldRe.FindStringSubmatch(line)
		if len(matches) != 2 {
			continue
		}
		foundField = true
		inlineValue := strings.TrimSpace(matches[1])
		if inlineValue != "" {
			raw = inlineValue
			break
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" {
				continue
			}
			if markdownHeadingRe.MatchString(next) {
				break
			}
			raw = next
			break
		}
		break
	}
	if !foundField || raw == "" {
		return ""
	}

	seen := make(map[string]bool)
	var normalized []string
	for _, part := range strings.Split(raw, ",") {
		ticket := normalizeTicketToken(part, fieldLabel)
		if ticket == "" || seen[ticket] {
			continue
		}
		seen[ticket] = true
		normalized = append(normalized, ticket)
	}
	return strings.Join(normalized, ",")
}

func normalizeTicketToken(token, fieldLabel string) string {
	ticket := strings.TrimSpace(token)
	if ticket == "" {
		return ""
	}
	ticket = strings.TrimLeft(ticket, "#")
	fieldPrefix := strings.TrimSpace(fieldLabel) + "-"
	if fieldPrefix != "-" && len(ticket) >= len(fieldPrefix) && strings.EqualFold(ticket[:len(fieldPrefix)], fieldPrefix) {
		ticket = ticket[len(fieldPrefix):]
	}
	ticket = strings.TrimSpace(strings.Trim(ticket, "[]"))
	if ticket == "" {
		return ""
	}
	return ticket
}

func extractProjectPath(webURL string) string {
	u, err := url.Parse(webURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if p == "-" && i >= 2 {
			return strings.Join(parts[:i], "/")
		}
	}
	return ""
}
