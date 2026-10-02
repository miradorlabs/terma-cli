package relay

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// heldTick is what the relay's ticker does to the store each second.
func heldTick(r *Relay) { r.flushHeld() }

// heldOnDisk is the store's batch files and their bytes.
func heldOnDisk(t *testing.T, dir string) (files, size int) {
	t.Helper()
	des, _ := os.ReadDir(filepath.Join(dir, heldDir))
	for _, de := range des {
		if strings.HasSuffix(de.Name(), heldSuffix) {
			info, err := de.Info()
			if err != nil {
				t.Fatal(err)
			}
			files++
			size += int(info.Size())
		}
	}
	return files, size
}

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
	second.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 0 {
		t.Fatalf("%d batch files outlived their parts' release", n)
	}
	if _, err := os.Stat(filepath.Join(dir, heldDir, heldIndex)); !os.IsNotExist(err) {
		t.Fatalf("the names index outlived the hold: %v", err)
	}
}

// A relay killed without its stop path loses only what it held since its last flush: the
// next relay on its directory takes the rest back and sends it once the claim lands.
func TestRelayHeldPartsSurviveACrash(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	dir := t.TempDir()
	opts := Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }}
	killed := newRelay(opts)
	srv := httptest.NewServer(killed.Handler())
	trace := []byte("fedcba9876543210")
	postProto(t, srv, "/v1/traces", sessionlessSpans(trace, 200))
	postProto(t, srv, "/v1/logs", namingLog(trace, "N"))
	postProto(t, srv, "/v1/logs", logsOf("N", 3))
	killed.flushHeld()                            // its ticker's last flush
	postProto(t, srv, "/v1/logs", logsOf("N", 1)) // after it: the crash takes this
	srv.Close()
	killed.cancelSend() // killed: no sweep, no stop, nothing more written

	f.advance(14 * time.Second)
	f.claim("N", claim.Claim{ProjectID: "p1", Tool: "codex"})
	next := newRelay(opts)
	if c := next.Stats().Snapshot().Counters; c["recovered_held.traces"] != 200 || c["recovered_held.logs"] != 4 {
		t.Fatalf("recovered after the crash: %v", c)
	}
	startRelay(t, next)
	next.sweep()
	waitFor(t, func() bool {
		c := next.Stats().Snapshot().Counters
		return c["forwarded.traces"] == 200 && c["forwarded.logs"] == 4
	})
	next.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 0 {
		t.Fatalf("%d batch files left after the release", n)
	}
}

// A part's copy goes with the flush after it leaves the hold, released or dropped, and a
// batch that keeps some of its parts is rewritten without the rest.
func TestRelayHeldStoreRemovesWhatLeaves(t *testing.T) {
	u := newUpstream(t)
	f := newFixture()
	dir := t.TempDir()
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	srv, _ := startRelay(t, r)
	postProto(t, srv, "/v1/logs", logsOf("X", 2, kv("prompt", "personal X")))
	postProto(t, srv, "/v1/logs", logsOf("Y", 2, kv("prompt", "personal Y")))
	r.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 2 {
		t.Fatalf("%d batch files for two sessions' held parts, want one each", n)
	}

	f.claim("X", claim.Claim{ProjectID: "p1"})
	r.sweep()
	r.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 1 {
		t.Fatalf("%d batch files after X was released, want Y's alone", n)
	}
	f.advance(2 * time.Minute)
	r.sweep()
	r.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 0 {
		t.Fatalf("%d batch files after Y expired", n)
	}
	if c := r.Stats().Snapshot().Counters; c["dropped.unclaimed_expired.logs"] != 2 || c["held_store_files_removed"] != 2 {
		t.Fatalf("stats = %v", c)
	}

	// One session's parts with different holds: the batch that keeps the start is rewritten.
	p1 := &part{signal: Logs, session: "Z", msg: logsOf("Z", 1, kv("prompt", "kept")), records: 1, start: true}
	p2 := &part{signal: Logs, session: "Z", msg: logsOf("Z", 1, kv("prompt", "goes")), records: 1}
	r.deliverMu.Lock()
	r.hold(p1)
	r.hold(p2)
	r.deliverMu.Unlock()
	r.flushHeld()
	f.advance(2 * time.Minute)
	r.sweep() // the ordinary part expires; the conversation start waits on
	r.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 1 {
		t.Fatalf("%d batch files, want the start's alone", n)
	}
	des, _ := os.ReadDir(filepath.Join(dir, heldDir))
	for _, de := range des {
		data, _ := os.ReadFile(filepath.Join(dir, heldDir, de.Name()))
		if strings.Contains(string(data), "goes") {
			t.Fatalf("%s still holds a dropped part", de.Name())
		}
	}
}

// The store holds no more than the hold does: a flood past maxHeldBytes leaves at most
// that much on disk, whatever was evicted along the way.
func TestRelayHeldStoreKeepsTheHoldsBound(t *testing.T) {
	f := newFixture()
	dir := t.TempDir()
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock})
	srv, _ := startRelay(t, r)
	big := strings.Repeat("x", 4<<20)
	for i := range 20 {
		postProto(t, srv, "/v1/logs", logsOf(fmt.Sprintf("flood-%d", i), 1, kv("blob", big)))
		r.flushHeld()
	}
	r.flushHeld()
	_, size := heldOnDisk(t, dir)
	if size > maxHeldBytes+64<<10 || size < 8<<20 {
		t.Fatalf("store holds %d bytes; the hold's bound is %d", size, maxHeldBytes)
	}
	if c := r.Stats().Snapshot().Counters; c["dropped.unclaimed_evicted.logs"] == 0 {
		t.Fatalf("stats = %v", c)
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

// A damaged batch gives back the parts before the damage; the rest is counted and removed.
func TestRelayHeldStoreSurvivesADamagedBatch(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, heldDir)
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	p := &part{signal: Logs, session: "C", msg: logsOf("C", 2), records: 2}
	body, _ := proto.Marshal(p.msg)
	frame, err := appendFrame(nil, metaOf(1, "C", heldPart{p: p, at: time.Unix(1_800_000_000, 0)}), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "00000000000000000001-000001.held"), append(frame, frame[:len(frame)/2]...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(held, "00000000000000000002-000002.held"), []byte("{not a frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	r := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Now: f.clock})
	if c := r.Stats().Snapshot().Counters; c["held_unreadable"] != 2 || c["recovered_held.logs"] != 2 {
		t.Fatalf("stats = %v", c)
	}
	r.flushHeld()
	if n, size := heldOnDisk(t, dir); n != 1 || size != len(frame) {
		t.Fatalf("store after the reload: %d files, %d bytes; want the whole frame alone", n, size)
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

// A crash between a compaction's write and its removal leaves a part in two batches; the
// next relay holds it once, and the extra copy goes with its next flush.
func TestRelayHeldStoreHoldsACopiedPartOnce(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, heldDir)
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	p := &part{signal: Logs, session: "C", msg: logsOf("C", 2), records: 2}
	body, _ := proto.Marshal(p.msg)
	frame, err := appendFrame(nil, metaOf(7, "C", heldPart{p: p, at: time.Unix(1_800_000_000, 0)}), body)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"00000000000000000001-000001.held", "00000000000000000002-000002.held"} {
		if err := os.WriteFile(filepath.Join(held, name), frame, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture()
	r := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Now: f.clock})
	if c := r.Stats().Snapshot().Counters; c["recovered_held.logs"] != 2 || r.heldN != 2 {
		t.Fatalf("held %d records: %v", r.heldN, c)
	}
	r.flushHeld()
	if n, _ := heldOnDisk(t, dir); n != 1 {
		t.Fatalf("%d batch files, want the one copy", n)
	}
}
