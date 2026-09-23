package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The archive and checksums URLs come from the release payload, which this code does
// not choose. The checksum cannot police them — it comes from the same payload — so
// the origin is pinned instead.
func TestGetRefusesNonGitHubOrigin(t *testing.T) {
	c := &Client{Version: "1.0.0"}
	for _, target := range []string{
		"https://evil.test/terma_Darwin_arm64.tar.gz",
		"http://github.com/terma.tar.gz",            // cleartext
		"https://github.com.evil.test/terma.tar.gz", // suffix trick
		"https://notgithub.com/terma.tar.gz",
		"file:///etc/passwd",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := c.get(context.Background(), target); err == nil {
				t.Fatalf("expected %q to be refused", target)
			} else if !strings.Contains(err.Error(), "must come from GitHub") &&
				!strings.Contains(err.Error(), "missing host") {
				t.Fatalf("expected an origin refusal, got %v", err)
			}
		})
	}
}

func TestDownloadOriginAllowsGitHub(t *testing.T) {
	c := &Client{Version: "1.0.0"}
	for _, target := range []string{
		"https://github.com/miradorlabs/terma-cli/releases/download/v1/terma.tar.gz",
		"https://objects.githubusercontent.com/foo",
		"https://release-assets.githubusercontent.com/bar",
	} {
		if err := c.checkDownloadOrigin(target); err != nil {
			t.Fatalf("%q should be allowed: %v", target, err)
		}
	}
}

// An explicitly configured base (tests, GitHub Enterprise) is a trusted origin too,
// because it was set by whoever built the client rather than named by a release.
func TestDownloadOriginAllowsConfiguredBase(t *testing.T) {
	c := &Client{Version: "1.0.0", BaseURL: "http://127.0.0.1:8080"}
	if err := c.checkDownloadOrigin("http://127.0.0.1:8080/archive"); err != nil {
		t.Fatalf("configured base should be allowed: %v", err)
	}
	if err := c.checkDownloadOrigin("http://127.0.0.1:9999/archive"); err == nil {
		t.Fatal("a different port is a different origin and must be refused")
	}
}

func TestRedirectCannotFetchFromAnotherOrigin(t *testing.T) {
	calls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, "unexpected") }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	c := &Client{BaseURL: source.URL, HTTP: source.Client()}
	if _, err := c.get(context.Background(), source.URL+"/asset"); err == nil {
		t.Fatal("untrusted redirect followed")
	}
	if calls != 0 {
		t.Fatal("untrusted destination contacted")
	}
}
