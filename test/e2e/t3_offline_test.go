package e2e

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The probe that picks T3's API waits out a server still attaching its routes (0.1.9 answers
// 503 until then) rather than take it for a build without the route.
func TestT3ServesJSONWaitsForRoutes(t *testing.T) {
	var shellCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/orchestration/shell":
			if shellCalls.Add(1) <= 2 {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			if r.Header.Get(t3ProtocolHeader) != t3Protocol {
				http.Error(w, "protocol", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"threads":[]}`))
		case "/api/orchestration/snapshot":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	x := &T3{t: t, url: srv.URL, token: "token"}
	if !x.servesJSON("/api/orchestration/shell", http.Header{t3ProtocolHeader: {t3Protocol}}) {
		t.Error("a shell route answering 503 while T3 starts, then JSON, read as no route")
	}
	if x.servesJSON("/api/orchestration/snapshot", nil) {
		t.Error("a route answered with the web app read as an API route")
	}
	if x.servesJSON("/api/elsewhere", nil) {
		t.Error("a 404 read as an API route")
	}
}
