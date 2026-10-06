package relay

import (
	"net/http/httptest"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// marked is a session a hook marked not collected, by processes pids (none: any sender).
func marked(since time.Time, pids ...int) claim.Claim {
	return claim.Claim{Tool: "claude-code", PIDs: pids, Placements: []claim.Placement{{Tool: "claude-code", PIDs: pids, Since: since}}}
}

// logsAt are n records of session stamped at.
func logsAt(session string, n int, at time.Time) *logspb.LogsData {
	m := logsOf(session, n)
	for _, lr := range m.ResourceLogs[0].ScopeLogs[0].LogRecords {
		lr.TimeUnixNano = uint64(at.UnixNano())
	}
	return m
}

// noMarkResolved is a Resolve that fails the test if a mark ever reaches it: a mark has no
// project to key, route or mint for.
func noMarkResolved(t *testing.T, u *upstream) func(claim.Claim) (Policy, error) {
	return func(c claim.Claim) (Policy, error) {
		if c.ProjectID == "" {
			t.Errorf("a mark was resolved: %+v", c)
		}
		if p, ok := allPolicies(u)[c.ProjectID]; ok {
			return p, nil
		}
		return Policy{}, ErrNoKey
	}
}

// A marked session's records are dropped as they arrive: never held, never on disk.
func TestNotCollectedIsDroppedOnArrival(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claim("P", marked(f.now))
	dir := t.TempDir()
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Resolve: noMarkResolved(t, u)})
	t.Cleanup(r.cancelSend)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	postProto(t, srv, "/v1/logs", logsOf("P", 3, kv("prompt", "personal")))
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.not_collected.logs"] != 3 || c["held_parts"] != 0 {
		t.Fatalf("stats = %v", c)
	}
	if r.heldN != 0 {
		t.Fatalf("%d records held", r.heldN)
	}
	if files := filesUnder(t, dir); len(files) != 0 {
		t.Fatalf("files for a marked session: %v", files)
	}
}

// What a session sent before its first hook marked it is dropped at the next sweep.
func TestNotCollectedDropsWhatWasHeld(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Resolve: noMarkResolved(t, u)})
	t.Cleanup(r.cancelSend)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	postProto(t, srv, "/v1/logs", logsOf("P", 2, kv("prompt", "personal")))
	r.sweep()
	if r.heldN != 2 {
		t.Fatalf("%d records held before the mark, want 2", r.heldN)
	}
	f.advance(time.Second)
	f.claim("P", marked(f.now))
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.not_collected.logs"] != 2 || c["dropped.unclaimed_expired.logs"] != 0 || r.heldN != 0 {
		t.Fatalf("stats = %v, held %d", c, r.heldN)
	}
}

// A thread marked in an unlisted repository, then claimed in a listed one, is collected
// from the move on: the marked process's records before the claim stay dropped, after it
// they go; a resumed run's process waits for its own placement, as any first export does.
func TestNotCollectedThenListedFromTheMove(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	t0 := pr.f.clock()
	pr.f.claim("M", marked(t0.Add(-time.Hour), 100))
	pr.send(100, "/v1/logs", logsAt("M", 1, t0))
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["dropped.not_collected.logs"] == 1 })

	t1 := t0.Add(time.Minute)
	listed := claim.Placement{ProjectID: "p1", PIDs: []int{100}, Since: t1}
	pr.f.claim("M", claim.Claim{ProjectID: "p1", PIDs: []int{100}, Placements: append(marked(t0.Add(-time.Hour), 100).Placements, listed)})
	pr.send(100, "/v1/logs", logsAt("M", 2, t1.Add(time.Second)))
	pr.send(100, "/v1/logs", logsAt("M", 1, t1.Add(-time.Second))) // late, from before the move
	waitFor(t, func() bool {
		c := pr.r.Stats().Snapshot().Counters
		return c["forwarded.logs"] == 2 && c["dropped.not_collected.logs"] == 2
	})

	pr.send(200, "/v1/logs", logsAt("M", 1, t1.Add(2*time.Minute))) // a resumed run, before its hook
	pr.r.sweep()
	if c := pr.r.Stats().Snapshot().Counters; c["held_parts"] != 1 {
		t.Fatalf("the resumed run's first record was not held: %v", c)
	}
	resumed := claim.Placement{ProjectID: "p1", PIDs: []int{200}, Since: t1.Add(3 * time.Minute)}
	pr.f.claim("M", claim.Claim{ProjectID: "p1", PIDs: []int{200}, Placements: append(marked(t0.Add(-time.Hour), 100).Placements, listed, resumed)})
	pr.r.sweep()
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.not_collected.logs"] != 2 {
		t.Fatalf("stats = %v", c)
	}
}

// Global mode's catch-all comes first: a mark left from repository mode changes nothing.
func TestNotCollectedLeavesGlobalModeAlone(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claim("P", marked(f.now))
	policies := allPolicies(u)
	policies["p-default"] = Policy{Endpoint: u.srv.URL, Key: "key-default"}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true },
		Resolve:  func(c claim.Claim) (Policy, error) { return policies[c.ProjectID], nil }})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	postProto(t, srv, "/v1/logs", logsOf("P", 2))
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	if c := r.Stats().Snapshot().Counters; sum(c, "dropped.") != 0 {
		t.Fatalf("stats = %v", c)
	}
}

// metricAt is codexMetric stamped at.
func metricAt(at time.Time) *metricspb.MetricsData {
	m := codexMetric()
	m.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[0].TimeUnixNano = uint64(at.UnixNano())
	return m
}

// A sessionless part of an exited process goes under the placement in force when it was
// sent, not the latest: what a process sent before it moved stays where it was sent from,
// a repository its team does not collect included.
func TestRelayAttributesAnExitedProcessByWhenItSent(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	t0 := pr.f.clock()
	t1 := t0.Add(time.Minute)
	pr.f.claim("U", claim.Claim{ProjectID: "p1", PIDs: []int{100}, Placements: []claim.Placement{
		{PIDs: []int{100}, Since: t0.Add(-time.Hour)}, // marked: an unlisted repository first
		{ProjectID: "p1", PIDs: []int{100}, Since: t1},
	}})
	pr.f.claim("L", claim.Claim{ProjectID: "p2", PIDs: []int{200}, Placements: []claim.Placement{
		{ProjectID: "p1", PIDs: []int{200}, Since: t0.Add(-time.Hour)},
		{ProjectID: "p2", PIDs: []int{200}, Since: t1},
	}})
	pr.send(100, "/v1/logs", logsAt("U", 1, t1.Add(time.Second)))
	pr.send(100, "/v1/metrics", metricAt(t0)) // before the move: the unlisted repository's
	pr.send(200, "/v1/logs", logsAt("L", 1, t1.Add(time.Second)))
	pr.send(200, "/v1/metrics", metricAt(t0)) // before the move: p1's
	pr.exit(100)
	pr.exit(200)
	waitFor(t, func() bool {
		c := pr.r.Stats().Snapshot().Counters
		return c["dropped.not_collected.metrics"] == 1 && c["forwarded.metrics"] == 1
	})
	if got := pr.metricsBy("Bearer key-p1"); len(got) != 1 {
		t.Fatalf("p1 got %d metrics, want the one sent before the move: %v", len(got), pr.r.Stats().Snapshot().Counters)
	}
}

// A part naming neither session nor trace, from a running process whose every session is
// marked not collected, is dropped on arrival: it could never leave. An unnamed span of
// that process waits to be named, as a shared process's next thread may name it, and goes
// once the process exits; a process that also named a claimed session waits as before.
func TestNotCollectedSessionlessParts(t *testing.T) {
	t.Parallel()
	pr := newProcRelay(t)
	t0 := pr.f.clock()
	pr.f.claim("U", marked(t0.Add(-time.Hour), 100, 300))
	pr.send(100, "/v1/logs", logsAt("U", 1, t0))
	pr.send(100, "/v1/metrics", metricAt(t0))
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["dropped.not_collected.metrics"] == 1 })

	pr.send(100, "/v1/traces", sessionlessSpans([]byte("0123456789abcdef"), 2))
	pr.send(300, "/v1/logs", logsAt("U", 1, t0))
	pr.send(300, "/v1/logs", logsAt("A", 1, t0)) // A is claimed: the process serves both
	pr.send(300, "/v1/metrics", metricAt(t0))
	pr.advance(time.Second)
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.not_collected.traces"] != 0 || c["dropped.not_collected.metrics"] != 1 {
		t.Fatalf("a part that may yet leave was dropped: %v", c)
	}
	pr.exit(100)
	waitFor(t, func() bool { return pr.r.Stats().Snapshot().Counters["dropped.not_collected.traces"] == 2 })
	if c := pr.r.Stats().Snapshot().Counters; c["dropped.not_collected.metrics"] != 1 || pr.r.heldN != 1 {
		t.Fatalf("stats = %v, held %d", c, pr.r.heldN)
	}
}
