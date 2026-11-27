package relay

import (
	"bytes"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// ledgerRouter routes as daemon.Teams does on a real run ledger: Mixed by the run covering
// the record's interval (naming that run), or by a Mixed placement; Send otherwise. Its
// recheck makes every entry wait until released, then judges it on a fresh ledger read.
func ledgerRouter(stateDir, endpoint string, released *atomic.Bool) *stubRouter {
	mixed := func(session string, start, end time.Time) (string, time.Time, bool) {
		run, at, ok := runs.Read(stateDir, session).MixedIn(start, end)
		if ok && at.IsZero() {
			at = time.Unix(0, 1)
		}
		return run, at, ok
	}
	return &stubRouter{
		active: true,
		routeFn: func(rec Record) Routed {
			if run, from, ok := mixed(rec.Session, rec.At, rec.EffectiveEnd()); ok || rec.Placement.Kind == claim.KindMixed {
				if !ok {
					run, from = rec.Placement.Run, rec.At
				}
				return Routed{Outcome: Drop, Why: WhyMixed, Blocker: "mixed:" + rec.Session, MixedAt: from, MixedRun: run}
			}
			return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr", Project: "proj", Account: rec.Account, Kind: "repository", Session: rec.Session, Run: rec.Placement.Run, At: rec.At, EndAt: rec.EndAt}}
		},
		recheckFn: func(_, _ string) Rechecker {
			return stubRechecker{gen: 1, entryFn: func(q Queued) Rechecked {
				if !released.Load() {
					return Rechecked{Verdict: Wait, Why: WhyBlocked}
				}
				if _, from, ok := mixed(q.Session, q.At, q.EffectiveEnd()); ok {
					return Rechecked{Verdict: Discard, Why: WhyMixed, MixedAt: from}
				}
				return Rechecked{Verdict: SendNow}
			}}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: endpoint, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
}

func bodyLog(session, body string, at time.Time) *logspb.LogsData {
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", session)}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(at.UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		}}}},
	}}}
}

// queuedBodies is what the team sender still holds, by body text.
func queuedBodies(t *testing.T, r *Relay) []string {
	t.Helper()
	var out []string
	for _, s := range r.allTeamSenders() {
		s.mu.Lock()
		es := slices.Clone(s.queue[Logs])
		s.mu.Unlock()
		for _, e := range es {
			b, err := s.readFile(e)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"paused", "runless", "resumed", "before", "crossed"} {
				if bytes.Contains(b, []byte(name)) {
					out = append(out, name)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// A session paused (its run Mixed) and resumed by a new process: a record of that process
// stamped before its new run began is judged Mixed, but sweeps only the paused run's queue
// (and entries with no run), never the new run's clean entries.
func TestMixedSweepIsScopedToTheMixedRun(t *testing.T) {
	t.Parallel()
	const old, runless, resumed = 100, 300, 200
	t0 := time.Now().Add(-time.Minute).Truncate(time.Second)

	t.Run("resume", func(t *testing.T) {
		t.Parallel()
		stateDir := t.TempDir()
		up := newSignInUpstream(t)
		var released atomic.Bool
		paused, err := runs.Start(stateDir, "s-resume", t0)
		if err != nil {
			t.Fatal(err)
		}
		var cl atomic.Pointer[claim.Claim]
		place := func(pls ...claim.Placement) {
			c := claim.Claim{Tool: "claude-code", Placements: pls}
			for _, p := range pls {
				c.PIDs = append(c.PIDs, p.PIDs...)
			}
			cl.Store(&c)
		}
		repo := func(pid int, run string, since time.Time) claim.Placement {
			return claim.Placement{Since: since, Kind: claim.KindRepository, ProjectID: "proj", Tool: "claude-code", Run: run, PIDs: []int{pid}}
		}
		place(repo(old, paused, t0), repo(runless, "", t0))
		r, _ := startTeamRelay(t, Options{
			Dir: t.TempDir(), SignIn: &testSignIn{account: accountA}, Router: ledgerRouter(stateDir, up.srv.URL, &released),
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if c := cl.Load(); session == "s-resume" && c != nil {
					return *c, true
				}
				return claim.Claim{}, false
			},
		})
		send := func(pid int, body string, at time.Time) {
			r.route(&part{signal: Logs, msg: bodyLog("s-resume", body, at), records: 1, pid: pid, session: "s-resume", account: accountA, arrived: time.Now(), at: at, startAt: at, endAt: at})
		}

		// Queued while the run is still clean: one under the soon-paused run, one with no run.
		send(old, "paused", t0.Add(12*time.Second))
		send(runless, "runless", t0.Add(15*time.Second))
		// The run is paused from t0+10s, and a new process resumes the session at t0+20s.
		if err := runs.MarkMixed(stateDir, "s-resume", t0.Add(10*time.Second), runs.Why{Reason: runs.UnknownReach}); err != nil {
			t.Fatal(err)
		}
		next, err := runs.Start(stateDir, "s-resume", t0.Add(20*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		place(repo(old, paused, t0), repo(runless, "", t0), repo(resumed, next, t0.Add(20*time.Second)))
		send(resumed, "resumed", t0.Add(21*time.Second))
		if got := queuedBodies(t, r); !slices.Equal(got, []string{"paused", "resumed", "runless"}) {
			t.Fatalf("queued before the stale record = %v", got)
		}

		// The new process's record stamped before its run began: placed in the new run,
		// inside the paused interval.
		send(resumed, "stale", t0.Add(19*time.Second))
		if got := queuedBodies(t, r); !slices.Equal(got, []string{"resumed"}) {
			t.Fatalf("queued after the stale record's Mixed verdict = %v, want [resumed]", got)
		}
		if got := r.Stats().Snapshot().Counters["dropped.mixed_run.logs"]; got != 3 {
			t.Fatalf("dropped.mixed_run.logs = %d, want 3 (paused, runless, stale)", got)
		}

		released.Store(true)
		r.OnStateChange("enr")
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
		var all []byte
		for _, b := range up.bodies() {
			all = append(all, b...)
		}
		if !bytes.Contains(all, []byte("resumed")) {
			t.Fatal("the resumed run's clean record never left")
		}
		for _, leaked := range []string{"paused", "runless", "stale"} {
			if bytes.Contains(all, []byte(leaked)) {
				t.Fatalf("%q left from the paused interval", leaked)
			}
		}
	})

	// A session that crosses organizations, with no resume: the sweep is as it was.
	t.Run("crossed", func(t *testing.T) {
		t.Parallel()
		stateDir := t.TempDir()
		up := newSignInUpstream(t)
		var released atomic.Bool
		only, err := runs.Start(stateDir, "s-cross", t0)
		if err != nil {
			t.Fatal(err)
		}
		c := claim.Claim{Tool: "claude-code", PIDs: []int{old}, Placements: []claim.Placement{
			{Since: t0, Kind: claim.KindRepository, ProjectID: "proj", Tool: "claude-code", Run: only, PIDs: []int{old}},
		}}
		r, _ := startTeamRelay(t, Options{
			Dir: t.TempDir(), SignIn: &testSignIn{account: accountA}, Router: ledgerRouter(stateDir, up.srv.URL, &released),
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) { return c, session == "s-cross" },
		})
		send := func(body string, at time.Time) {
			r.route(&part{signal: Logs, msg: bodyLog("s-cross", body, at), records: 1, pid: old, session: "s-cross", account: accountA, arrived: time.Now(), at: at, startAt: at, endAt: at})
		}
		send("before", t0.Add(5*time.Second))
		send("crossed", t0.Add(12*time.Second))
		if err := runs.MarkMixed(stateDir, "s-cross", t0.Add(10*time.Second), runs.Why{Reason: runs.Crossed}); err != nil {
			t.Fatal(err)
		}
		send("after", t0.Add(13*time.Second))
		if got := queuedBodies(t, r); !slices.Equal(got, []string{"before"}) {
			t.Fatalf("queued after crossing = %v, want [before]", got)
		}
		released.Store(true)
		r.OnStateChange("enr")
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
		var all []byte
		for _, b := range up.bodies() {
			all = append(all, b...)
		}
		if !bytes.Contains(all, []byte("before")) || bytes.Contains(all, []byte("crossed")) || bytes.Contains(all, []byte("after")) {
			t.Fatalf("upstream got %q, want only the record before the crossing", all)
		}
	})
}
