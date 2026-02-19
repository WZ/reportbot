package slackbot

type githubSearchResponse struct {
	TotalCount int            `json:"total_count"`
	Items      []githubPRItem `json:"items"`
}

type githubPRItem struct {
	Title         string         `json:"title"`
	HTMLURL       string         `json:"html_url"`
	State         string         `json:"state"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
	ClosedAt      string         `json:"closed_at"`
	User          githubUser     `json:"user"`
	Labels        []githubLabel  `json:"labels"`
	PullRequest   *githubPRLinks `json:"pull_request"`
	RepositoryURL string         `json:"repository_url"`
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
