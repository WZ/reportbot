package main

import "testing"

func TestExternalHTTPClientTimeout(t *testing.T) {
	if externalHTTPClient == nil {
		t.Fatal("externalHTTPClient must not be nil")
	}
	if externalHTTPClient.Timeout <= 0 {
		t.Fatalf("externalHTTPClient timeout must be set, got %s", externalHTTPClient.Timeout)
	}
	if externalHTTPClient.Timeout != externalHTTPTimeout {
		t.Fatalf("externalHTTPClient timeout = %s, want %s", externalHTTPClient.Timeout, externalHTTPTimeout)
	}
}
