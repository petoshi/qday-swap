package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceEndpointValidation(t *testing.T) {
	token := strings.Repeat("a", 64)
	for _, endpoint := range []string{
		"http://127.0.0.1:42000/open/" + token,
		"http://[::1]:42000/open/" + token,
		"http://localhost:42000/open/" + token,
	} {
		if err := validateInstanceEndpoint(endpoint); err != nil {
			t.Fatalf("valid endpoint %q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"https://127.0.0.1:42000/open/" + token,
		"http://example.com:42000/open/" + token,
		"http://127.0.0.1:42000/open/short",
		"http://127.0.0.1:42000/open/" + token + "?leak=1",
	} {
		if err := validateInstanceEndpoint(endpoint); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
}

func TestSecondInstanceFindsRunningInterface(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "qday-swap")
	first, err := acquireInstance(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	endpoint := "http://127.0.0.1:42000/open/" + strings.Repeat("b", 64)
	if err := first.Publish(endpoint); err != nil {
		t.Fatal(err)
	}
	second, err := acquireInstance(dataDir)
	if second != nil {
		_ = second.Close()
		t.Fatal("second instance acquired the same lock")
	}
	var running *alreadyRunningError
	if !errors.As(err, &running) || running.URL != endpoint {
		t.Fatalf("second instance error = %#v", err)
	}
	endpointPath := first.endpointPath
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpointPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint survived owner shutdown: %v", err)
	}
}
