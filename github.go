package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type githubSearchResponse struct {
	TotalCount int              `json:"total_count"`
	Items      []githubPRItem   `json:"items"`
}

type githubPRItem struct {
	Title       string          `json:"title"`
	HTMLURL     string          `json:"html_url"`
	State       string          `json:"state"`      // "open" or "closed"
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	ClosedAt    string          `json:"closed_at"`
	User        githubUser      `json:"user"`
	Labels      []githubLabel   `json:"labels"`
	PullRequest *githubPRLinks  `json:"pull_request"`
	RepositoryURL string        `json:"repository_url"` // e.g. "https://api.github.com/repos/org/repo"
}

type githubUser struct {
	Login string `json:"login"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubPRLinks struct {
	MergedAt string `json:"merged_at"`
}

func FetchGitHubPRs(cfg Config, from, to time.Time) ([]GitHubPR, error) {
	fromStr := from.Format("2006-01-02")
	toStr := to.Format("2006-01-02")
	scope := buildScopeQualifier(cfg)

	var allPRs []GitHubPR

	// Query 1: Merged PRs in the date range.
	mergedQuery := fmt.Sprintf("type:pr is:merged merged:%s..%s %s", fromStr, toStr, scope)
	log.Printf("github fetch merged query=%s", mergedQuery)
	mergedItems, err := searchGitHubPRs(cfg.GitHubToken, mergedQuery)
	if err != nil {
		return nil, fmt.Errorf("searching merged PRs: %w", err)
	}
	for _, item := range mergedItems {
		allPRs = append(allPRs, convertGitHubItem(item, "merged"))
	}

	// Query 2: Open PRs updated since the start of the range.
	openQuery := fmt.Sprintf("type:pr is:open updated:>=%s %s", fromStr, scope)
	log.Printf("github fetch open query=%s", openQuery)
	openItems, err := searchGitHubPRs(cfg.GitHubToken, openQuery)
	if err != nil {
		return nil, fmt.Errorf("searching open PRs: %w", err)
	}
	for _, item := range openItems {
		pr := convertGitHubItem(item, "open")
		// Filter: only include open PRs updated within the date range.
		// If UpdatedAt is zero (missing or failed to parse), skip this PR,
		// since we cannot be sure it falls within the requested range.
		if pr.UpdatedAt.IsZero() {
			continue
		}
		if pr.UpdatedAt.Before(from) {
			continue
		}
		if !pr.UpdatedAt.Before(to) {
			continue
		}
		allPRs = append(allPRs, pr)
	}

	log.Printf("github fetch done total=%d (merged=%d open=%d)", len(allPRs), len(mergedItems), len(allPRs)-len(mergedItems))
	return allPRs, nil
}

func searchGitHubPRs(token, query string) ([]githubPRItem, error) {
	var all []githubPRItem
	page := 1

	for {
		apiURL := fmt.Sprintf("https://api.github.com/search/issues?q=%s&per_page=100&page=%d",
			url.QueryEscape(query), page)

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("executing request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
		}

		var result githubSearchResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}

		all = append(all, result.Items...)

		if len(result.Items) < 100 {
			break
		}
		page++
	}

	return all, nil
}

func convertGitHubItem(item githubPRItem, derivedState string) GitHubPR {
	createdAt, _ := time.Parse(time.RFC3339, item.CreatedAt)
	updatedAt, _ := time.Parse(time.RFC3339, item.UpdatedAt)
	closedAt, _ := time.Parse(time.RFC3339, item.ClosedAt)

	var mergedAt time.Time
	if item.PullRequest != nil && item.PullRequest.MergedAt != "" {
		mergedAt, _ = time.Parse(time.RFC3339, item.PullRequest.MergedAt)
	}
	// Fallback: Search Issues API doesn't include pull_request.merged_at; for
	// merged PRs, approximate merged time using closed_at when mergedAt is zero.
	if mergedAt.IsZero() && derivedState == "merged" && !closedAt.IsZero() {
		mergedAt = closedAt
	}

	var labels []string
	for _, l := range item.Labels {
		labels = append(labels, l.Name)
	}

	return GitHubPR{
		Title:        item.Title,
		Author:       item.User.Login,
		AuthorName:   item.User.Login, // Search API doesn't return display name
		HTMLURL:      item.HTMLURL,
		MergedAt:     mergedAt,
		UpdatedAt:    updatedAt,
		CreatedAt:    createdAt,
		ClosedAt:     closedAt,
		State:        derivedState,
		Labels:       labels,
		RepoFullName: extractRepoFullName(item.RepositoryURL),
	}
}

func extractRepoFullName(repoURL string) string {
	// repoURL is like "https://api.github.com/repos/org/repo-name"
	u, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Expected: ["repos", "org", "repo-name"]
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[1] + "/" + parts[2]
	}
	return ""
}

func buildScopeQualifier(cfg Config) string {
	if len(cfg.GitHubRepos) > 0 {
		var parts []string
		for _, repo := range cfg.GitHubRepos {
			parts = append(parts, "repo:"+repo)
		}
		return strings.Join(parts, " ")
	}
	if cfg.GitHubOrg != "" {
		return "org:" + cfg.GitHubOrg
	}
	return ""
}
