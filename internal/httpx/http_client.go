package httpx

import (
	"crypto/tls"
	"net/http"
	"time"
)

const defaultExternalHTTPTimeout = 90 * time.Second

var externalHTTPClient = &http.Client{
	Timeout: defaultExternalHTTPTimeout,
}

func ConfigureExternalHTTPClient(timeoutSeconds int, tlsSkipVerify bool) time.Duration {
	timeout := defaultExternalHTTPTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	externalHTTPClient.Timeout = timeout
	if tlsSkipVerify {
		externalHTTPClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return timeout
}

func ExternalHTTPClient() *http.Client {
	return externalHTTPClient
}
