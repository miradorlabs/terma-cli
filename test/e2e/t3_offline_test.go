package e2e

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// t3Server answers the routes the driver probes as a build of one shape: snapshot and shell are
// what it answers GET /api/orchestration/snapshot and /shell with ("json", "html", "400" for
// another protocol), the shell after starting answers of 503 while it attaches its routes.
func t3Server(t *testing.T, snapshot, shell string, starting int32) *T3 {
	t.Helper()
	var calls atomic.Int32
	answer := func(w http.ResponseWriter, as string) {
		switch as {
		case "json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"threads":[]}`))
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html>"))
		case "400":
			http.Error(w, "unsupported orchestration protocol", http.StatusBadRequest)
		default:
			http.NotFound(w, nil)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= starting {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/api/orchestration/snapshot":
			answer(w, snapshot)
		case "/api/orchestration/shell":
			if r.Header.Get(t3ProtocolHeader) != t3Protocol {
				answer(w, "400")
				return
			}
			answer(w, shell)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &T3{t: t, url: srv.URL, token: "token"}
}

// The driver speaks the API a build serves, once its routes answer: 0.0.x serves its
// orchestration snapshot (and a shell route of its own), 0.1.x its shell snapshot alone, and a
// build on another protocol neither.
func TestT3API(t *testing.T) {
	for _, c := range []struct {
		name, snapshot, shell string
		starting              int32
		want                  string
	}{
		{"0.0.x", "json", "json", 0, "0.0"},
		{"0.1.x", "html", "json", 0, "0.1"},
		{"0.1.9 attaching its routes", "html", "json", 2, "0.1"},
		{"another protocol", "html", "400", 0, ""},
		{"neither route", "html", "html", 0, ""},
	} {
		if got := t3Server(t, c.snapshot, c.shell, c.starting).api(); got != c.want {
			t.Errorf("%s: api = %q, want %q", c.name, got, c.want)
		}
	}
}

// A run has ended when its status is one T3 settles it with, never a live one.
func TestT3SettledRuns(t *testing.T) {
	for _, live := range []string{"idle", "preparing", "queued", "starting", "running", "waiting"} {
		if t3SettledRuns[live] {
			t.Errorf("%s read as settled", live)
		}
	}
	for _, settled := range []string{"completed", "interrupted", "failed", "cancelled", "rolled_back"} {
		if !t3SettledRuns[settled] {
			t.Errorf("%s read as live", settled)
		}
	}
}
