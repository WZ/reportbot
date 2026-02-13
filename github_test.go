package main

import (
	"testing"
	"time"
)

func TestExtractRepoFullName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://api.github.com/repos/myorg/myrepo", "myorg/myrepo"},
		{"https://api.github.com/repos/acme/widget-service", "acme/widget-service"},
		{"https://api.github.com/repos/a/b/extra", "a/b"},
		{"", ""},
		{"not-a-url", ""},
		{"https://api.github.com/users/foo", ""},
	}
	for _, tt := range tests {
		got := extractRepoFullName(tt.input)
		if got != tt.want {
			t.Errorf("extractRepoFullName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildScopeQualifier(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "org only",
			cfg:  Config{GitHubOrg: "myorg"},
			want: "org:myorg",
		},
		{
			name: "repos only",
			cfg:  Config{GitHubRepos: []string{"myorg/repo1", "myorg/repo2"}},
			want: "repo:myorg/repo1 repo:myorg/repo2",
		},
		{
			name: "repos take precedence over org",
			cfg:  Config{GitHubOrg: "myorg", GitHubRepos: []string{"other/repo"}},
			want: "repo:other/repo",
		},
		{
			name: "empty config",
			cfg:  Config{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildScopeQualifier(tt.cfg)
			if got != tt.want {
				t.Errorf("buildScopeQualifier() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertGitHubItem(t *testing.T) {
	t.Run("merged PR", func(t *testing.T) {
		item := githubPRItem{
			Title:   "Fix login bug",
			HTMLURL: "https://github.com/myorg/myrepo/pull/42",
			State:   "closed",
			User:    githubUser{Login: "alice"},
			Labels:  []githubLabel{{Name: "bugfix"}, {Name: "urgent"}},
			PullRequest: &githubPRLinks{
				MergedAt: "2025-01-15T10:30:00Z",
			},
			CreatedAt:     "2025-01-14T08:00:00Z",
			UpdatedAt:     "2025-01-15T10:30:00Z",
			ClosedAt:      "2025-01-15T10:30:00Z",
			RepositoryURL: "https://api.github.com/repos/myorg/myrepo",
		}

		pr := convertGitHubItem(item, "merged")

		if pr.Title != "Fix login bug" {
			t.Errorf("Title = %q, want %q", pr.Title, "Fix login bug")
		}
		if pr.Author != "alice" {
			t.Errorf("Author = %q, want %q", pr.Author, "alice")
		}
		if pr.AuthorName != "alice" {
			t.Errorf("AuthorName = %q, want %q", pr.AuthorName, "alice")
		}
		if pr.HTMLURL != "https://github.com/myorg/myrepo/pull/42" {
			t.Errorf("HTMLURL = %q, want the PR URL", pr.HTMLURL)
		}
		if pr.State != "merged" {
			t.Errorf("State = %q, want %q", pr.State, "merged")
		}
		if pr.MergedAt.IsZero() {
			t.Error("MergedAt should not be zero for merged PR")
		}
		if len(pr.Labels) != 2 {
			t.Errorf("Labels count = %d, want 2", len(pr.Labels))
		}
		if pr.RepoFullName != "myorg/myrepo" {
			t.Errorf("RepoFullName = %q, want %q", pr.RepoFullName, "myorg/myrepo")
		}
	})

	t.Run("open PR", func(t *testing.T) {
		item := githubPRItem{
			Title:         "Add feature X",
			HTMLURL:       "https://github.com/myorg/myrepo/pull/99",
			State:         "open",
			User:          githubUser{Login: "bob"},
			CreatedAt:     "2025-01-10T08:00:00Z",
			UpdatedAt:     "2025-01-15T14:00:00Z",
			RepositoryURL: "https://api.github.com/repos/myorg/myrepo",
		}

		pr := convertGitHubItem(item, "open")

		if pr.State != "open" {
			t.Errorf("State = %q, want %q", pr.State, "open")
		}
		if pr.MergedAt.IsZero() != true {
			t.Error("MergedAt should be zero for open PR")
		}
	})
}

func TestMapPRStatus(t *testing.T) {
	tests := []struct {
		state string
		want  string
	}{
		{"open", "in progress"},
		{"merged", "done"},
		{"closed", "done"},
	}
	for _, tt := range tests {
		pr := GitHubPR{State: tt.state}
		got := mapPRStatus(pr)
		if got != tt.want {
			t.Errorf("mapPRStatus(state=%q) = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestPRReportedAt(t *testing.T) {
	loc := time.UTC
	ts := func(s string) time.Time {
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}

	t.Run("open PR uses UpdatedAt", func(t *testing.T) {
		pr := GitHubPR{
			State:     "open",
			UpdatedAt: ts("2025-01-15T14:00:00Z"),
			MergedAt:  time.Time{},
			CreatedAt: ts("2025-01-10T08:00:00Z"),
		}
		got := prReportedAt(pr, loc)
		if !got.Equal(ts("2025-01-15T14:00:00Z")) {
			t.Errorf("prReportedAt(open) = %v, want UpdatedAt", got)
		}
	})

	t.Run("merged PR uses MergedAt", func(t *testing.T) {
		pr := GitHubPR{
			State:     "merged",
			UpdatedAt: ts("2025-01-15T14:00:00Z"),
			MergedAt:  ts("2025-01-15T10:30:00Z"),
			CreatedAt: ts("2025-01-10T08:00:00Z"),
		}
		got := prReportedAt(pr, loc)
		if !got.Equal(ts("2025-01-15T10:30:00Z")) {
			t.Errorf("prReportedAt(merged) = %v, want MergedAt", got)
		}
	})

	t.Run("fallback to CreatedAt", func(t *testing.T) {
		pr := GitHubPR{
			State:     "merged",
			CreatedAt: ts("2025-01-10T08:00:00Z"),
		}
		got := prReportedAt(pr, loc)
		if !got.Equal(ts("2025-01-10T08:00:00Z")) {
			t.Errorf("prReportedAt(no merged/updated) = %v, want CreatedAt", got)
		}
	})

	t.Run("fallback to now", func(t *testing.T) {
		pr := GitHubPR{State: "merged"}
		before := time.Now()
		got := prReportedAt(pr, loc)
		after := time.Now()
		if got.Before(before) || got.After(after) {
			t.Errorf("prReportedAt(all zero) = %v, expected roughly now", got)
		}
	})
}
