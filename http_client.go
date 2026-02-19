package main

import (
	"net/http"
	"time"
)

const externalHTTPTimeout = 30 * time.Second

var externalHTTPClient = &http.Client{
	Timeout: externalHTTPTimeout,
}
