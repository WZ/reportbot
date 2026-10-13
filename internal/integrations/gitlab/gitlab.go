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
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"author"`
	Labels     []string `json:"labels"`
	References struct {
		Full string `json:"full"`
	} `json:"references"`
}

var markdownHeadingRe = regexp.MustCompile(`^\s*#{1,6}\s+\S`)

func FetchMRs(src Source, from, to time.Time) ([]GitLabMR, error) {
	// UTC: a literal Z here would mislabel local wall-clock time and shift the window.
	since := from.UTC().Format(time.RFC3339)
	groupID := url.PathEscape(src.GroupID)
	ticketFieldLabel := strings.TrimSpace(src.RefTicketLabel)

	var allMRs []GitLabMR
	page := 1
	log.Printf("gitlab fetch start source=%s group=%s since=%s", src.Name, src.GroupID, since)

	for {
		apiURL := fmt.Sprintf("%s/api/v4/groups/%s/merge_requests?state=all&updated_after=%s&per_page=100&page=%d",
			strings.TrimRight(src.URL, "/"), groupID, since, page)
		log.Printf("gitlab fetch page=%d", page)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("PRIVATE-TOKEN", src.Token)

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
				AuthorID:    mr.Author.ID,
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

// ParseTicketIDs reads the ticket field (ref_ticket_label of the source) of a merge
// request description.
func ParseTicketIDs(description, fieldLabel string) string {
	return parseTicketIDsFromDescription(description, fieldLabel)
}

type gitlabCommitResponse struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Message       string `json:"message"`
	AuthorName    string `json:"author_name"`
	AuthorEmail   string `json:"author_email"`
	AuthoredDate  string `json:"authored_date"`
	CommittedDate string `json:"committed_date"`
	WebURL        string `json:"web_url"`
}

// mergeCommitRe matches merge and revert commits, which are integration noise
// rather than reportable work.
var mergeCommitRe = regexp.MustCompile(`(?i)^(merge( branch| pull request| remote-tracking branch| .+ into .+)?|revert)\b`)

// FetchBranchCommits lists a project branch's commits with committed_date inside
// [from, to), skipping merge and revert commits. Pagination follows page numbers.
func FetchBranchCommits(src Source, from, to time.Time) ([]GitLabCommit, error) {
	// UTC: a literal Z here would mislabel local wall-clock time and shift the window.
	since := from.UTC().Format(time.RFC3339)
	project := url.PathEscape(src.Project)

	var all []GitLabCommit
	page := 1
	log.Printf("gitlab fetch start source=%s project=%s branch=%s since=%s", src.Name, src.Project, src.Branch, since)

	for {
		apiURL := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits?ref_name=%s&since=%s&per_page=100&page=%d",
			strings.TrimRight(src.URL, "/"), project, url.QueryEscape(src.Branch), since, page)
		log.Printf("gitlab fetch commits page=%d", page)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("PRIVATE-TOKEN", src.Token)

		resp, err := externalHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching commits: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, string(body))
		}

		var commits []gitlabCommitResponse
		if err := json.Unmarshal(body, &commits); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		for _, cm := range commits {
			committed := parseRFC3339(cm.CommittedDate)
			if committed.IsZero() {
				committed = parseRFC3339(cm.AuthoredDate)
			}
			if committed.IsZero() {
				log.Printf("gitlab fetch: skipping commit %s with unparseable dates %q / %q", cm.ID, cm.CommittedDate, cm.AuthoredDate)
				continue
			}
			if committed.Before(from) || !committed.Before(to) {
				continue
			}
			if mergeCommitRe.MatchString(strings.TrimSpace(cm.Title)) {
				continue
			}
			all = append(all, GitLabCommit{
				SHA:         cm.ID,
				Title:       cm.Title,
				Message:     cm.Message,
				AuthorName:  cm.AuthorName,
				AuthorEmail: cm.AuthorEmail,
				CommittedAt: committed,
				WebURL:      cm.WebURL,
			})
		}

		if len(commits) < 100 {
			break
		}
		page++
	}

	log.Printf("gitlab fetch commits done total=%d", len(all))
	return all, nil
}

// ParseCommitTicketIDs reads ticket IDs from a commit title or message, in the
// "<label>[- ]<digits>" forms (e.g. "Mantis 0987654: fix" or "Mantis-0987654").
func ParseCommitTicketIDs(text, fieldLabel string) string {
	fieldLabel = strings.TrimSpace(fieldLabel)
	if fieldLabel == "" || strings.TrimSpace(text) == "" {
		return ""
	}
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(fieldLabel) + `\s*[-: ]\s*#?(\d+)\b`)
	var ids []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if id := m[1]; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ",")
}

func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
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
