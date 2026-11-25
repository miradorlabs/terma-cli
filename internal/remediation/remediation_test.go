package remediation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A machine is warned when its release is older than the campaign's minimum, or when the
// campaign wants a person's sign-in and it has none; source, dirty and snapshot builds,
// and a minimum that is no release, are never behind.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                string
		local, minimum      string
		requireUser, signed bool
		behind, needsUser   bool
	}{
		{"older release", "0.41.2", "0.43.0", false, true, true, false},
		{"older release, v prefix", "v0.41.2", "0.43.0", false, true, true, false},
		{"minimum with v prefix", "0.41.2", "v0.43.0", false, true, true, false},
		{"same release", "0.43.0", "0.43.0", false, true, false, false},
		{"newer release", "0.44.1", "0.43.0", false, true, false, false},
		{"prerelease of the minimum", "0.43.0-rc.1", "0.43.0", false, true, true, false},
		{"dev build", "dev", "0.43.0", false, true, false, false},
		{"git-describe build", "0.41.2-3-gabcdef1", "0.43.0", false, true, false, false},
		{"dirty build", "0.41.2-dirty", "0.43.0", false, true, false, false},
		{"snapshot build", "0.41.3-next", "0.43.0", false, true, false, false},
		{"empty version", "", "0.43.0", false, true, false, false},
		{"malformed minimum", "0.41.2", "latest", false, true, false, false},
		{"no minimum", "0.41.2", "", false, true, false, false},
		{"user session required, signed in", "0.44.0", "0.43.0", true, true, false, false},
		{"user session required, not signed in", "0.44.0", "0.43.0", true, false, false, true},
		{"user session required on a dev build", "dev", "0.43.0", true, false, false, true},
		{"both", "0.41.2", "0.43.0", true, false, true, true},
		{"not required, not signed in", "0.44.0", "0.43.0", false, false, false, false},
	} {
		n := Evaluate(&Campaign{MinimumClientVersion: c.minimum, RequireUserSession: c.requireUser}, c.local, c.signed)
		if n.Behind != c.behind || n.NeedsUserSession != c.needsUser || n.Warn() != (c.behind || c.needsUser) {
			t.Errorf("%s: behind=%v needsUser=%v, want %v %v", c.name, n.Behind, n.NeedsUserSession, c.behind, c.needsUser)
		}
		if n.Warn() == (n.Summary() == "") {
			t.Errorf("%s: warn=%v but summary %q", c.name, n.Warn(), n.Summary())
		}
	}
	if Evaluate(nil, "0.1.0", false).Warn() {
		t.Error("no campaign warned")
	}
}

// The summary names the required release, this one and the commands, in that order.
func TestSummaryAndLines(t *testing.T) {
	t.Parallel()
	n := Evaluate(&Campaign{Title: "Update terma", Steps: []string{"Run terma update", "Run terma setup"}, MinimumClientVersion: "0.43.0"}, "0.41.2", true)
	if got := n.Summary(); got != "terma 0.43.0 or later is required, this is 0.41.2 — run `terma update`, then `terma setup`" {
		t.Errorf("summary = %q", got)
	}
	if got := strings.Join(n.Lines(), "|"); got != "Update terma|1. Run terma update|2. Run terma setup" {
		t.Errorf("lines = %q", got)
	}
}

// Fetch asks the public route with terma's User-Agent and no credential, and reads both
// answers; control characters never reach the terminal.
func TestFetch(t *testing.T) {
	t.Setenv("TERMA_REMEDIATION_STUB", "")
	var got *http.Request
	body := `{"campaign":{"id":"c1","title":"Update\u001b[2J terma","message":"m","steps":["one","\u0007",""],"minimum_client_version":" 0.43.0 ","require_user_session":true,"updated_at":"2026-11-20T10:00:00Z"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := Fetch(t.Context(), srv.URL+"/", "0.41.2")
	if err != nil || c == nil {
		t.Fatalf("Fetch = %+v, %v", c, err)
	}
	if got.URL.Path != "/v1/remediation-campaigns/active" || got.URL.Query().Get("product") != "PRODUCT_TERMA_AI" {
		t.Errorf("asked %s", got.URL)
	}
	if got.Header.Get("User-Agent") != "terma-cli/0.41.2" || got.Header.Get("Authorization") != "" {
		t.Errorf("headers = %v", got.Header)
	}
	if c.ID != "c1" || c.Title != "Update [2J terma" || len(c.Steps) != 1 || c.Steps[0] != "one" ||
		c.MinimumClientVersion != "0.43.0" || !c.RequireUserSession || c.UpdatedAt.IsZero() {
		t.Errorf("campaign = %+v", c)
	}

	body = `{"campaign":null}`
	if c, err := Fetch(t.Context(), srv.URL, "0.41.2"); c != nil || err != nil {
		t.Errorf("no campaign = %+v, %v", c, err)
	}
}

// An older platform without the route, a failing one, a malformed answer and one too slow
// are errors, never a campaign: the caller stays silent.
func TestFetchFailures(t *testing.T) {
	t.Setenv("TERMA_REMEDIATION_STUB", "")
	for _, c := range []struct {
		name  string
		serve http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"malformed", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"campaign":`)) }},
		{"html", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>`)) }},
		{"slow", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}},
	} {
		srv := httptest.NewServer(c.serve)
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		got, err := Fetch(ctx, srv.URL, "0.41.2")
		cancel()
		srv.Close()
		if got != nil || err == nil {
			t.Errorf("%s: Fetch = %+v, %v; want an error", c.name, got, err)
		}
	}
	if c, err := Fetch(t.Context(), "http://127.0.0.1:1", "0.41.2"); c != nil || err == nil {
		t.Errorf("offline: Fetch = %+v, %v", c, err)
	}
}

// The stub stands in for the route and reaches nothing.
func TestFetchStub(t *testing.T) {
	t.Setenv("TERMA_REMEDIATION_STUB", `{"campaign":{"id":"c1","minimum_client_version":"0.43.0"}}`)
	if c, err := Fetch(t.Context(), "http://127.0.0.1:1", "dev"); err != nil || c == nil || c.ID != "c1" {
		t.Fatalf("Fetch = %+v, %v", c, err)
	}
	t.Setenv("TERMA_REMEDIATION_STUB", `nope`)
	if c, err := Fetch(t.Context(), "http://127.0.0.1:1", "dev"); c != nil || err == nil {
		t.Fatalf("malformed stub: Fetch = %+v, %v", c, err)
	}
}

// Fetch bounds itself: a platform that answers and then stalls the body costs Timeout, not
// the caller's patience.
func TestFetchTimesOutOnItsOwn(t *testing.T) {
	t.Setenv("TERMA_REMEDIATION_STUB", "")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"campaign":`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	start := time.Now()
	c, err := Fetch(context.Background(), srv.URL, "0.41.2")
	if took := time.Since(start); c != nil || err == nil || took < Timeout || took > Timeout+2*time.Second {
		t.Fatalf("Fetch = %+v, %v after %s; want an error after %s", c, err, took, Timeout)
	}
}
