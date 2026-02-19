package httpx

import (
	"testing"
	"time"
)

func TestExternalHTTPClientTimeout(t *testing.T) {
	if externalHTTPClient == nil {
		t.Fatal("externalHTTPClient must not be nil")
	}
	if externalHTTPClient.Timeout <= 0 {
		t.Fatalf("externalHTTPClient timeout must be set, got %s", externalHTTPClient.Timeout)
	}
	if externalHTTPClient.Timeout != defaultExternalHTTPTimeout {
		t.Fatalf("externalHTTPClient timeout = %s, want %s", externalHTTPClient.Timeout, defaultExternalHTTPTimeout)
	}
}

func TestConfigureExternalHTTPClient(t *testing.T) {
	original := externalHTTPClient.Timeout
	t.Cleanup(func() {
		externalHTTPClient.Timeout = original
	})

	got := ConfigureExternalHTTPClient(0)
	if got != defaultExternalHTTPTimeout {
		t.Fatalf("ConfigureExternalHTTPClient(0) = %s, want %s", got, defaultExternalHTTPTimeout)
	}
	if externalHTTPClient.Timeout != defaultExternalHTTPTimeout {
		t.Fatalf("configured timeout = %s, want %s", externalHTTPClient.Timeout, defaultExternalHTTPTimeout)
	}

	got = ConfigureExternalHTTPClient(120)
	if got != 120*time.Second {
		t.Fatalf("ConfigureExternalHTTPClient(120) = %s, want %s", got, 120*time.Second)
	}
	if externalHTTPClient.Timeout != 120*time.Second {
		t.Fatalf("configured timeout = %s, want %s", externalHTTPClient.Timeout, 120*time.Second)
	}
}
