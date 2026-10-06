package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
)

// setup's check-in reports the beat as sent, no team key here yet, another failure,
// or no relay at all.
func TestRelayCheckIn(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_ = free.Close()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if ok, what := CheckIn(t.Context(), stateDir); ok || !strings.Contains(what, "did not answer") || time.Since(start) > 3*time.Second {
		t.Fatalf("no relay: %v %q after %v", ok, what, time.Since(start))
	}
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
		says   string
	}{
		{200, `{}`, true, "reported to your organization"},
		{502, `{"error":"no key for this team on this machine"}`, true, "once a repository is connected"},
		{502, `{"error":"invalid token"}`, false, "invalid token"},
	} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/heartbeat" || r.URL.Query().Get("reason") != "setup" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})}
		go func() { _ = srv.Serve(ln) }()
		unlock, _ := flock.TryLock(filepath.Join(dir, LockFile)) // "running"
		ok, what := CheckIn(t.Context(), stateDir)
		if unlock != nil {
			unlock()
		}
		_ = srv.Close()
		if ok != tc.ok || !strings.Contains(what, tc.says) {
			t.Errorf("%d %s: %v %q", tc.status, tc.body, ok, what)
		}
	}
}
