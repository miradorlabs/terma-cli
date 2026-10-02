package relay

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A relay restarted while a session waits for its claim loses none of it: Codex claims
// 13–24 s in, and a restart in that window dropped its startup spans for good. The spans'
// trace was named, and their process seen, before the stop; the claim lands after it.
func TestRelayHeldPartsSurviveARestart(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	dir := t.TempDir()
	var exited atomic.Bool
	opts := Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID:      func(int) (int, bool) { return 100, true },
		ProcessAlive: func(int) bool { return !exited.Load() },
		Resolve:      func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }}
	first := newRelay(opts)
	srv, stop := startRelay(t, first)
	trace := []byte("0123456789abcdef")
	postProto(t, srv, "/v1/traces", sessionlessSpans(trace, 3))
	postProto(t, srv, "/v1/logs", namingLog(trace, "N"))
	postProto(t, srv, "/v1/metrics", codexMetric()) // names nothing: goes by its process, which named N
	stop()
	c := first.Stats().Snapshot().Counters
	if c["held_at_exit.traces"] != 3 || c["held_at_exit.logs"] != 1 || c["held_at_exit.metrics"] != 1 || sum(c, "dropped.") != 0 {
		t.Fatalf("first relay: %v", c)
	}

	f.advance(14 * time.Second)
	f.claim("N", claim.Claim{ProjectID: "p1", Tool: "codex"})
	exited.Store(true)
	second := newRelay(opts)
	c = second.Stats().Snapshot().Counters
	if c["recovered_held.traces"] != 3 || c["recovered_held.logs"] != 1 || c["recovered_held.metrics"] != 1 {
		t.Fatalf("second relay recovered: %v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, heldDir)); !os.IsNotExist(err) {
		t.Fatalf("the held parts stayed on disk once taken back: %v", err)
	}
	startRelay(t, second)
	second.sweep() // the trace and the claim place the spans and the log
	f.advance(exitGrace + time.Second)
	second.sweep() // the process is gone: its metric goes to its one session
	waitFor(t, func() bool {
		c := second.Stats().Snapshot().Counters
		return c["forwarded.traces"] == 3 && c["forwarded.logs"] == 1 && c["forwarded.metrics"] == 1
	})
	if c := second.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("second relay dropped: %v", c)
	}
}

// A restart neither extends a hold nor lifts its bounds: a part's hold runs from when it
// was first held, and one past it is dropped by the next relay for the usual reason.
func TestRelayHoldRunsOnAcrossARestart(t *testing.T) {
	f := newFixture()
	dir := t.TempDir()
	opts := Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock}
	first := newRelay(opts)
	srv, stop := startRelay(t, first)
	postProto(t, srv, "/v1/logs", logsOf("C", 2))
	stop()
	f.advance(2 * time.Minute)
	second := newRelay(opts)
	second.sweep()
	c := second.Stats().Snapshot().Counters
	if c["recovered_held.logs"] != 2 || c["dropped.unclaimed_expired.logs"] != 2 {
		t.Fatalf("second relay: %v", c)
	}
	if _, quiet := second.Idle(); !quiet {
		t.Fatal("an expired part kept the relay busy")
	}
}

// A held part that cannot be read back is counted and removed, never sent or kept.
func TestRelayDiscardsAnUnreadableHeldPart(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, heldDir)
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "00000000000000000001-000000.held"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRelay(Options{Dir: dir, Token: token})
	if c := r.Stats().Snapshot().Counters; c["held_unreadable"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	if _, err := os.Stat(held); !os.IsNotExist(err) {
		t.Fatalf("held directory left behind: %v", err)
	}
}

// A part held across a restart keeps the policy it arrived under: the next relay, already
// in global mode, still sends none of it.
func TestRelaySwitchToGlobalAcrossARestartSendsNothingHeldFromBefore(t *testing.T) {
	g := &globalSwitch{u: newUpstream(t)}
	f := newFixture()
	opts := Options{Dir: t.TempDir(), Token: token, Hold: 2 * time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: g.catchAll, Resolve: g.resolve}
	first := newRelay(opts)
	srv, stop := startRelay(t, first)
	postProto(t, srv, "/v1/logs", logsOf("U", 2))
	stop()
	g.on.Store(true)
	f.claim("U", claim.Claim{ProjectID: "p-default"})
	second := newRelay(opts)
	second.sweep()
	f.advance(3 * time.Minute)
	second.sweep()
	c := second.Stats().Snapshot().Counters
	if c["recovered_held.logs"] != 2 || c["dropped.policy_widened.logs"] != 2 || c["forwarded.logs"] != 0 {
		t.Fatalf("second relay: %v", c)
	}
}
