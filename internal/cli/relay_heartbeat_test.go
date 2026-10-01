package cli

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
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// The heartbeat names this terma's build and setup, never whose machine it is; the machine
// id is random and stable.
func TestHeartbeatFactsNameNoOne(t *testing.T) {
	dir := relaySandbox(t)
	facts := testApp.relayHeartbeat(dir).Facts()
	for _, k := range []string{"terma.version", "terma.os", "terma.arch", "terma.machine_id", "terma.install", "terma.mode", "terma.relay.service"} {
		if _, ok := facts[k]; !ok {
			t.Errorf("no %s in %v", k, facts)
		}
	}
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	for k, v := range facts {
		s := fmt.Sprint(v)
		if host != "" && strings.Contains(s, host) || home != "" && strings.Contains(s, home) || strings.Contains(s, "@") {
			t.Errorf("%s = %q names the machine or its owner", k, s)
		}
	}
	id := facts["terma.machine_id"]
	if id == "" || testApp.relayHeartbeat(dir).Facts()["terma.machine_id"] != id {
		t.Fatalf("machine id %q is not stable", id)
	}
}

// setup's check-in reports the beat as sent, endpoint not there yet (the stub's 404),
// another failure, or no relay at all.
func TestRelayCheckIn(t *testing.T) {
	dir := relaySandbox(t)
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	start := time.Now()
	if ok, what := daemon.CheckIn(t.Context()); ok || !strings.Contains(what, "did not answer") || time.Since(start) > 3*time.Second {
		t.Fatalf("no relay: %v %q after %v", ok, what, time.Since(start))
	}
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
		says   string
	}{
		{200, `{}`, true, "reported to your organization"},
		{502, `{"error":"request failed with status 404"}`, true, "does not take check-ins yet"},
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
		unlock, _ := flock.TryLock(filepath.Join(dir, daemon.LockFile)) // "running"
		ok, what := daemon.CheckIn(t.Context())
		if unlock != nil {
			unlock()
		}
		_ = srv.Close()
		if ok != tc.ok || !strings.Contains(what, tc.says) {
			t.Errorf("%d %s: %v %q", tc.status, tc.body, ok, what)
		}
	}
}
