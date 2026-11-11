package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeWeb serves pages by their real URL from a local server: a client it makes sends every
// request there. A URL it has no page for is a 404.
type fakeWeb struct {
	mu    sync.Mutex
	pages map[string]string
	hits  map[string]int
}

func newFakeWeb(t *testing.T, pages map[string]string) (*client, *fakeWeb) {
	t.Helper()
	w := &fakeWeb{pages: pages, hits: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		u := r.URL.Query().Get("u")
		w.mu.Lock()
		w.hits[u]++
		body, ok := w.pages[u]
		w.mu.Unlock()
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := newClient()
	c.wait = time.Millisecond
	c.rewrite = func(u string) string { return srv.URL + "/?u=" + url.QueryEscape(u) }
	return c, w
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
