package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type gitlabMRResponse struct {
	Title    string `json:"title"`
	WebURL   string `json:"web_url"`
	MergedAt string `json:"merged_at"`
	Author   struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"author"`
	Labels     []string `json:"labels"`
	References struct {
		Full string `json:"full"`
	} `json:"references"`
}

func FetchMergedMRs(cfg Config, from, to time.Time) ([]GitLabMR, error) {
	since := from.Format("2006-01-02T15:04:05Z")
	groupID := url.PathEscape(cfg.GitLabGroupID)

	var allMRs []GitLabMR
	page := 1

	for {
		apiURL := fmt.Sprintf("%s/api/v4/groups/%s/merge_requests?state=merged&updated_after=%s&per_page=100&page=%d",
			strings.TrimRight(cfg.GitLabURL, "/"), groupID, since, page)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("PRIVATE-TOKEN", cfg.GitLabToken)

		resp, err := http.DefaultClient.Do(req)
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
				continue
			}
			if mergedAt.Before(from) || !mergedAt.Before(to) {
				continue
			}

			projectPath := extractProjectPath(mr.WebURL)

			allMRs = append(allMRs, GitLabMR{
				Title:       mr.Title,
				Author:      mr.Author.Username,
				AuthorName:  mr.Author.Name,
				WebURL:      mr.WebURL,
				MergedAt:    mergedAt,
				Labels:      mr.Labels,
				ProjectPath: projectPath,
			})
		}

		if len(mrs) < 100 {
			break
		}
		page++
	}

	return allMRs, nil
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
