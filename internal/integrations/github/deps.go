package github

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/httpx"
)

type Config = config.Config
type GitHubPR = domain.GitHubPR

var externalHTTPClient = httpx.ExternalHTTPClient()
