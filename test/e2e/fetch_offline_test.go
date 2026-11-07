package e2e

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A download a server fails for a while (504 under load) is tried again; one the request
// fails (404) is not.
func TestFetchRetriesOffline(t *testing.T) {
	fetchBackoff = time.Millisecond
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case r.URL.Path == "/missing":
			http.NotFound(w, r)
		case n < 3:
			w.WriteHeader(http.StatusGatewayTimeout)
		default:
			_, _ = w.Write([]byte("build"))
		}
	}))
	defer srv.Close()
	body, err := fetch(srv.URL + "/build")
	if err != nil || string(body) != "build" || calls.Load() != 3 {
		t.Fatalf("fetch = %q, %v after %d calls; want the build on the third", body, err, calls.Load())
	}
	calls.Store(0)
	if _, err := fetch(srv.URL + "/missing"); !errors.Is(err, errNotFound) || calls.Load() != 1 {
		t.Fatalf("a 404 was fetched %d times (err %v); want once, errNotFound", calls.Load(), err)
	}
	// A server that keeps failing is a failure, never an asset the release lacks.
	calls.Store(-100)
	if _, err := fetch(srv.URL + "/build"); err == nil || errors.Is(err, errNotFound) {
		t.Fatalf("a server failing every try: %v; want an error that is not errNotFound", err)
	}
}
