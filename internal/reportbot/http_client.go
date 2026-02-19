package reportbot

import (
	"net/http"
	"time"
)

const defaultExternalHTTPTimeout = 90 * time.Second

var externalHTTPClient = &http.Client{
	Timeout: defaultExternalHTTPTimeout,
}

func ConfigureExternalHTTPClient(timeoutSeconds int) time.Duration {
	timeout := defaultExternalHTTPTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	externalHTTPClient.Timeout = timeout
	return timeout
}
