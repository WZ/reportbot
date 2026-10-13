package gitlab

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/httpx"
)

type Config = config.Config
type Source = config.GitLabSource
type GitLabMR = domain.GitLabMR
type GitLabCommit = domain.GitLabCommit

var externalHTTPClient = httpx.ExternalHTTPClient()
