package gitlab

import (
	"reportbot/internal/config"
	"reportbot/internal/domain"
	"reportbot/internal/httpx"
)

type Config = config.Config
type GitLabMR = domain.GitLabMR

var externalHTTPClient = httpx.ExternalHTTPClient()
