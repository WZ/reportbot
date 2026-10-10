package llm

import (
	"reportbot/internal/config"
	"reportbot/internal/httpx"
)

type Config = config.Config

var externalHTTPClient = httpx.ExternalHTTPClient()
