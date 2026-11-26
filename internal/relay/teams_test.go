package relay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

type stubRouter struct {
	mu          sync.Mutex
	active      bool
	unknownPers bool
	gen         int64
	routeCalls  int
	routeFn     func(Record) Routed
	recheckFn   func(enrollment, account string) Rechecker
	resolveFn   func(enrollment, tool, account string) (Policy, error)
	selectedFn  func(enrollment, account string) bool
}

func (r *stubRouter) Active() bool            { r.mu.Lock(); defer r.mu.Unlock(); return r.active }
func (r *stubRouter) UnknownToPersonal() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.unknownPers }
func (r *stubRouter) Route(rec Record) Routed {
	r.mu.Lock()
	r.routeCalls++
	fn := r.routeFn
	r.mu.Unlock()
	if fn != nil {
		return fn(rec)
	}
	return Routed{Outcome: Hold, Why: WhyUnknown}
}
func (r *stubRouter) Recheck(enrollment, account string) Rechecker {
	r.mu.Lock()
	fn := r.recheckFn
	gen := r.gen
	r.mu.Unlock()
	if fn != nil {
		return fn(enrollment, account)
	}
	return stubRechecker{gen: gen, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
}
func (r *stubRouter) Resolve(enrollment, tool, account string) (Policy, error) {
	r.mu.Lock()
	fn := r.resolveFn
	r.mu.Unlock()
	if fn != nil {
		return fn(enrollment, tool, account)
	}
	return Policy{}, os.ErrNotExist
}
func (r *stubRouter) Selected(enrollment, account string) bool {
	r.mu.Lock()
	fn := r.selectedFn
	r.mu.Unlock()
	if fn != nil {
		return fn(enrollment, account)
	}
	return true
}

type stubRechecker struct {
	stateID string
	gen     int64
	waits   bool
	waitRes Rechecked
	entryFn func(Queued) Rechecked
}

func (s stubRechecker) StateID() string              { return s.stateID }
func (s stubRechecker) Generation() int64            { return s.gen }
func (s stubRechecker) TeamWaits() (Rechecked, bool) { return s.waitRes, s.waits }
func (s stubRechecker) Entry(q Queued) Rechecked     { return s.entryFn(q) }

func startTeamRelay(t *testing.T, opts Options) (*Relay, *httptest.Server) {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Token == "" {
		opts.Token = token
	}
	r := newRelay(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(func() {
		srv.Close()
		cancel()
		<-done
	})
	return r, srv
}

func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Invariant 1: interval [start, end] across spans, logs, and all 5 metric types;
// straddling spans and interval metrics drop at Mixed (!endAt.Before(mixedAt)) while
// clean pre-Mixed records in the same batch still leave.
func TestIntervalTimestampsAndMixedStraddleSplit(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := t0.Add(10 * time.Second)
	mixedAt := t0.Add(20 * time.Second)
	t3 := t0.Add(30 * time.Second)
	ns := func(tm time.Time) uint64 { return uint64(tm.UnixNano()) }

	// Logs: [min, max] of TimeUnixNano and ObservedTimeUnixNano.
	logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
			{TimeUnixNano: ns(t0), ObservedTimeUnixNano: ns(t1)},      // clean [t0, t1]
			{TimeUnixNano: ns(t1), ObservedTimeUnixNano: ns(mixedAt)}, // touches mixedAt -> drop
		}}},
	}}}
	s, e := intervalOf(logs)
	if !s.Equal(t0) || !e.Equal(mixedAt) {
		t.Fatalf("logs interval = [%v, %v], want [%v, %v]", s, e, t0, mixedAt)
	}
	before, after := splitBeforeMixed(&part{signal: Logs, msg: logs, records: 2, at: s, endAt: e}, mixedAt)
	if before == nil || after == nil || before.records != 1 || after.records != 1 || !before.endAt.Equal(t1) || !after.endAt.Equal(mixedAt) {
		t.Fatalf("logs splitBeforeMixed = (%+v, %+v), want 1 before (end=%v) and 1 mixed (end=%v)", before, after, t1, mixedAt)
	}

	// Spans: [StartTimeUnixNano, EndTimeUnixNano]; straddling span [t1, t3] drops while [t0, t1] passes.
	traces := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{Name: "clean", StartTimeUnixNano: ns(t0), EndTimeUnixNano: ns(t1)},
			{Name: "straddle", StartTimeUnixNano: ns(t1), EndTimeUnixNano: ns(t3)},
		}}},
	}}}
	s, e = intervalOf(traces)
	if !s.Equal(t0) || !e.Equal(t3) {
		t.Fatalf("spans interval = [%v, %v], want [%v, %v]", s, e, t0, t3)
	}
	before, after = splitBeforeMixed(&part{signal: Traces, msg: traces, records: 2, at: s, endAt: e}, mixedAt)
	if before == nil || after == nil || before.records != 1 || after.records != 1 {
		t.Fatalf("spans splitBeforeMixed = (%+v, %+v), want 1/1", before, after)
	}
	if got := before.msg.(*tracepb.TracesData).ResourceSpans[0].ScopeSpans[0].Spans[0].GetName(); got != "clean" {
		t.Fatalf("before span = %q, want clean", got)
	}
	if got := after.msg.(*tracepb.TracesData).ResourceSpans[0].ScopeSpans[0].Spans[0].GetName(); got != "straddle" {
		t.Fatalf("after span = %q, want straddle", got)
	}

	// Metrics: all 5 metric types (delta Sum/Histogram/ExponentialHistogram keep [start, time],
	// while Gauge and Summary place by TimeUnixNano).
	delta := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	metrics := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{AggregationTemporality: delta, DataPoints: []*metricspb.NumberDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
			{Name: "gauge", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(mixedAt)},
			}}}},
			{Name: "hist", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: delta, DataPoints: []*metricspb.HistogramDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
			{Name: "exp", Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{AggregationTemporality: delta, DataPoints: []*metricspb.ExponentialHistogramDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
			{Name: "summary", Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: []*metricspb.SummaryDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
		}}},
	}}}
	s, e = intervalOf(metrics)
	if !s.Equal(t0) || !e.Equal(t3) {
		t.Fatalf("metrics interval = [%v, %v], want [%v, %v]", s, e, t0, t3)
	}
	before, after = splitBeforeMixed(&part{signal: Metrics, msg: metrics, records: 10, at: s, endAt: e}, mixedAt)
	if before == nil || after == nil || before.records != 5 || after.records != 5 {
		t.Fatalf("metrics splitBeforeMixed = (%+v, %+v), want 5 before and 5 mixed", before, after)
	}

	// End-to-end through Relay: clean span leaves, straddling span drops as mixed_run.
	u := newSignInUpstream(t)
	si := &testSignIn{account: accountA}
	router := &stubRouter{
		active: true,
		routeFn: func(rec Record) Routed {
			if rec.EffectiveEnd().IsZero() || !rec.EffectiveEnd().Before(mixedAt) {
				return Routed{Outcome: Drop, Why: WhyMixed, Blocker: "mixed:" + rec.Session, MixedAt: mixedAt}
			}
			return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_1", Project: "proj_1", Account: accountA, Kind: "team", Session: rec.Session, At: rec.At, EndAt: rec.EndAt}}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	r, srv := startTeamRelay(t, Options{
		SignIn: si, Router: router,
		Lookup: func(string, time.Time) (claim.Claim, bool) {
			return claim.Claim{ProjectID: "proj_1", Placements: []claim.Placement{
				{Kind: claim.KindRepository, ProjectID: "proj_1", Since: t0},
				{Kind: claim.KindMixed, Since: mixedAt},
			}}, true
		},
	})
	if code := post(t, srv, "/v1/traces", mustMarshal(t, traces), "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("post traces = %d, want 200", code)
	}
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	snap := r.Stats().Snapshot()
	if snap.Counters["dropped.mixed_run.traces"] != 1 || snap.Blocked["mixed:s1"] != 1 {
		t.Fatalf("mixed drop stats = %v blocked = %v, want 1", snap.Counters, snap.Blocked)
	}
	var got tracepb.TracesData
	if err := proto.Unmarshal(u.bodies()[0], &got); err != nil {
		t.Fatal(err)
	}
	if name := got.ResourceSpans[0].ScopeSpans[0].Spans[0].GetName(); name != "clean" {
		t.Fatalf("delivered span = %q, want clean", name)
	}
}

// Invariant 2: unknownHold vs destination-pinned pinnedHold; pinned entries NEVER re-route
// through Router.Route (only Recheck -> Wait/SendNow/Discard), and policyHold 15m / policy_timeout
// stays in memory and counts in losses.json.
func TestPinnedHoldNeverReroutesAndPolicyTimeout(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	u := newSignInUpstream(t)
	si := &testSignIn{account: accountA}
	var verdict atomic.Int32 // 0=Wait(Pending), 1=SendNow, 2=Wait(Blocked)
	var recheckCalls atomic.Int32
	router := &stubRouter{
		active:      true,
		unknownPers: true,
		routeFn: func(rec Record) Routed {
			if !rec.Placed {
				return Routed{Outcome: Hold, Why: WhyUnknown}
			}
			why := WhyPending
			if verdict.Load() == 2 {
				why = WhyBlocked
			}
			return Routed{
				Outcome: Hold,
				Why:     why,
				Blocker: "pending:b1",
				Queued:  Queued{Enrollment: "enr_team", Project: "proj_team", Account: accountA, Kind: "team", HoldKey: "b1", Session: rec.Session},
			}
		},
		recheckFn: func(enrollment, _ string) Rechecker {
			recheckCalls.Add(1)
			return stubRechecker{
				gen: 1,
				entryFn: func(q Queued) Rechecked {
					switch verdict.Load() {
					case 1:
						return Rechecked{Verdict: SendNow}
					case 2:
						return Rechecked{Verdict: Wait, Why: WhyBlocked, Blocker: "team:" + enrollment}
					default:
						return Rechecked{Verdict: Wait, Why: WhyPending, Blocker: "pending:" + q.HoldKey}
					}
				},
			}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}

	dir := t.TempDir()
	r, srv := startTeamRelay(t, Options{
		Dir: dir, SignIn: si, Router: router, Now: clock,
		Hold: 30 * time.Second,
		Lookup: func(s string, _ time.Time) (claim.Claim, bool) {
			if s == "s_pin" || s == "s_pol" {
				return claim.Claim{ProjectID: "proj_team", Placements: []claim.Placement{{Kind: claim.KindRepository, ProjectID: "proj_team", Since: now}}}, true
			}
			return claim.Claim{}, false
		},
	})

	// 1) Export for s_pin -> Router.Route assigns enr_team with Outcome=Hold -> lands in pinnedHold.
	post(t, srv, "/v1/logs", mustMarshal(t, oneSessionLog("s_pin", "pinned")), "application/x-protobuf", token, false)
	router.mu.Lock()
	routesAfterFirst := router.routeCalls
	router.mu.Unlock()
	if routesAfterFirst != 1 {
		t.Fatalf("routeCalls = %d, want 1", routesAfterFirst)
	}

	// Sweep while still Wait(Pending): must call ONLY Recheck, NEVER Route again!
	r.sweep()
	router.mu.Lock()
	routesAfterSweep := router.routeCalls
	router.mu.Unlock()
	if routesAfterSweep != 1 {
		t.Fatalf("pinnedHold re-entered Router.Route (%d calls, want 1)", routesAfterSweep)
	}
	if recheckCalls.Load() == 0 {
		t.Fatal("expected sweepPinned to call Recheck")
	}

	// Now Recheck returns SendNow -> releases to pinned enr_team without calling Route!
	verdict.Store(1)
	r.sweep()
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	router.mu.Lock()
	routesAfterRelease := router.routeCalls
	router.mu.Unlock()
	if routesAfterRelease != 1 {
		t.Fatalf("pinnedHold release called Router.Route (%d calls, want 1)", routesAfterRelease)
	}
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" proj_team" {
		t.Fatalf("sent to %q, want Bearer token-%s proj_team", got, accountA)
	}

	// 2) policyHold (15m): team blocked stays in memory past Hold (30s), QuiesceDroppingPolicyWaits
	// ignores it, and advancing past policyHold (15m) drops it and records policy_timeout in losses.json.
	verdict.Store(2)
	post(t, srv, "/v1/logs", mustMarshal(t, oneSessionLog("s_pol", "blocked")), "application/x-protobuf", token, false)
	advance(time.Minute)
	r.sweep()
	if !r.Holding() {
		t.Fatal("expected policy-waiting pinned part to stay held past Hold (30s)")
	}
	if !r.QuiesceDroppingPolicyWaits(0) {
		t.Fatal("QuiesceDroppingPolicyWaits should not wait on policy-blocked pinnedHold parts")
	}
	advance(policyHold)
	r.sweep()
	if r.Holding() {
		t.Fatal("expected policy-waiting pinned part to drop after policyHold (15m)")
	}
	losses, err := ReadOutboxLosses(dir)
	if err != nil || losses.Counts[lostPolicyTimeout] != 1 {
		t.Fatalf("losses = %+v (err=%v), want policy_timeout=1", losses, err)
	}
}

// Invariant 3: .teams/@<account>/<enrollment>/<tool>/ isolation from pre-cutover layout,
// PurgeOldOutbox drops pre-cutover parts into losses.json (no_destination) leaving .teams/ intact,
// and QueuedSessions lists .teams/ sessions for queue-aware runs.Prune.
func TestTeamOutboxIsolationPurgeAndQueuedSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0).UTC()
	o := outbox{dir: dir}

	// Seed pre-cutover outbox files (both legacy un-laned and @account laned).
	oldA := route{account: accountA, project: "p_old", tool: "claude-code", repo: noRepo}
	oldB := route{project: "p_legacy", tool: noTool, repo: noRepo}
	if err := o.put(oldA, claim.Claim{}.Repository, newEntry(now, Logs, 3), []byte("old-a")); err != nil {
		t.Fatal(err)
	}
	if err := o.put(oldB, claim.Claim{}.Repository, newEntry(now, Traces, 2), []byte("old-b")); err != nil {
		t.Fatal(err)
	}

	// Seed routed .teams/ outbox parts.
	rt := teamRoute{account: accountA, enrollment: "enr_1", tool: "claude-code"}
	q := Queued{Enrollment: "enr_1", Project: "proj_1", Account: accountA, Kind: "team", Session: "sess_keep"}
	e := newEntry(now, Logs, 4)
	if err := o.putTeam(rt, q, e, []byte("team-body")); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, ".teams", "@"+accountA, "enr_1", "claude-code", e.name)
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected .teams/ path %s: %v", wantPath, err)
	}

	// PurgeOldOutbox drops old layout (5 records -> no_destination) and leaves .teams/ untouched.
	dropped, err := PurgeOldOutbox(dir, now)
	if err != nil || dropped != 5 {
		t.Fatalf("PurgeOldOutbox = (%d, %v), want (5, nil)", dropped, err)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("PurgeOldOutbox touched .teams/ file: %v", err)
	}
	losses, err := ReadOutboxLosses(dir)
	if err != nil || losses.Counts[lostNoDestination] != 5 {
		t.Fatalf("losses = %+v (err=%v), want no_destination=5", losses, err)
	}

	// QueuedSessions discovers sess_keep from .teams/.
	sessions, err := QueuedSessions(dir)
	if err != nil || !sessions["sess_keep"] || len(sessions) != 1 {
		t.Fatalf("QueuedSessions = (%v, %v), want {sess_keep:true}", sessions, err)
	}
}

// Invariant 4: fenced send admission — (a) laneActive first, (b) user token + X-Mirador-Project +
// terma.user.id vs server-key no_person with NO X-Mirador-Project & NO terma.user.id,
// (c) pre-POST versioned admission fence with NO lock across HTTP, (d) dropDeselected scoped to active lane.
func TestFencedSendAdmissionAndAccountScopedDeselect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now().UTC()
	o := outbox{dir: dir}

	// 4a) Server-key no_person lane: uses pol.Key, NO X-Mirador-Project header, strips terma.user.id.
	var gotAuth, gotProj string
	var gotBody []byte
	var gotMu sync.Mutex
	keySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMu.Lock()
		gotAuth, gotProj, gotBody = r.Header.Get("Authorization"), r.Header.Get(ProjectHeader), b
		gotMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer keySrv.Close()

	// Seed a log with a bogus pre-existing terma.user.id to confirm filterQueued strips it on no_person.
	rawLog := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv(semconv.TermaUserIDKey, "stale_user")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hi"}}}}}},
	}}}
	keyDir := t.TempDir()
	if err := (outbox{dir: keyDir}).putTeam(
		teamRoute{sendas.NoPersonLane, "enr_srv", "claude-code"},
		Queued{Enrollment: "enr_srv", Project: "proj_srv", Account: sendas.NoPersonLane, Kind: "team"},
		newEntry(now, Logs, 1), mustMarshal(t, rawLog),
	); err != nil {
		t.Fatal(err)
	}
	keyRouter := &stubRouter{
		active: true,
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: keySrv.URL, Key: "srv-key-1", Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	startTeamRelay(t, Options{Dir: keyDir, SignIn: &testSignIn{account: sendas.NoPersonLane}, Router: keyRouter})
	waitFor(t, func() bool { gotMu.Lock(); defer gotMu.Unlock(); return gotAuth != "" })
	gotMu.Lock()
	authHdr, projHdr, bodyBytes := gotAuth, gotProj, gotBody
	gotMu.Unlock()
	if authHdr != "Bearer srv-key-1" || projHdr != "" {
		t.Fatalf("no_person send headers: auth=%q project=%q, want Bearer srv-key-1 and empty project", authHdr, projHdr)
	}
	var sentLogs logspb.LogsData
	if err := proto.Unmarshal(bodyBytes, &sentLogs); err != nil {
		t.Fatal(err)
	}
	if uid := attr(sentLogs.ResourceLogs[0].Resource.Attributes, semconv.TermaUserIDKey); uid != "" {
		t.Fatalf("no_person batch kept terma.user.id=%q, want stripped", uid)
	}

	// 4b) User lane stamps terma.user.id strictly for rt.account and sends X-Mirador-Project.
	userU := newSignInUpstream(t)
	userDir := t.TempDir()
	if err := (outbox{dir: userDir}).putTeam(
		teamRoute{accountA, "enr_u", "claude-code"},
		Queued{Enrollment: "enr_u", Project: "proj_u", Account: accountA, Kind: "team"},
		newEntry(now, Logs, 1), mustMarshal(t, rawLog),
	); err != nil {
		t.Fatal(err)
	}
	userRouter := &stubRouter{
		active: true,
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: userU.srv.URL, Account: account, User: "usr_alice", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	startTeamRelay(t, Options{Dir: userDir, SignIn: &testSignIn{account: accountA}, Router: userRouter})
	waitFor(t, func() bool { return len(userU.requests()) == 1 })
	if got := userU.requests()[0]; got != "Bearer token-"+accountA+" proj_u" {
		t.Fatalf("user send = %q, want Bearer token-%s proj_u", got, accountA)
	}
	var userSent logspb.LogsData
	if err := proto.Unmarshal(userU.bodies()[0], &userSent); err != nil {
		t.Fatal(err)
	}
	if uid := attr(userSent.ResourceLogs[0].Resource.Attributes, semconv.TermaUserIDKey); uid != "usr_alice" {
		t.Fatalf("user batch terma.user.id = %q, want usr_alice", uid)
	}

	// 4c) Pre-POST versioned admission fence aborts on concurrent Mixed/generation change without calling HTTP,
	// AND holds no lock across HTTP.Do.
	var httpCalls atomic.Int32
	inHTTP := make(chan struct{})
	releaseHTTP := make(chan struct{})
	fenceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if httpCalls.Add(1) == 1 {
			close(inHTTP)
			<-releaseHTTP
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fenceSrv.Close()

	var recheckCount atomic.Int32
	si := &testSignIn{account: accountA}
	fenceRouter := &stubRouter{
		active: true,
		gen:    1,
		recheckFn: func(_, _ string) Rechecker {
			n := recheckCount.Add(1)
			// First call (nextBatch): SendNow at gen=1.
			// Second call (pre-POST fence): Discard (Mixed) at gen=2 -> must abort BEFORE HTTP.Do!
			if n == 2 {
				return stubRechecker{gen: 2, entryFn: func(Queued) Rechecked {
					return Rechecked{Verdict: Discard, Why: WhyMixed, Blocker: "mixed:s_fence"}
				}}
			}
			return stubRechecker{gen: 1, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: fenceSrv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	fenceDir := t.TempDir()
	if err := (outbox{dir: fenceDir}).putTeam(
		teamRoute{accountA, "enr_f", "claude-code"},
		Queued{Enrollment: "enr_f", Project: "proj_f", Account: accountA, Kind: "team", Session: "s_fence"},
		newEntry(now, Logs, 1), mustMarshal(t, rawLog),
	); err != nil {
		t.Fatal(err)
	}
	fr, fsrv := startTeamRelay(t, Options{Dir: fenceDir, SignIn: si, Router: fenceRouter})
	waitForCounters(t, fr, func(c map[string]int) bool { return c["dropped.mixed_run.logs"] == 1 })
	if httpCalls.Load() != 0 {
		t.Fatalf("pre-POST fence failed: HTTP.Do called %d times, want 0", httpCalls.Load())
	}

	// Verify no lock is held across HTTP.Do: block one send in HTTP.Do and verify export + OnStateChange + Stats don't deadlock.
	recheckCount.Store(10)
	fenceRouter.mu.Lock()
	fenceRouter.routeFn = func(rec Record) Routed {
		return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_f", Project: "proj_f", Account: accountA, Kind: "team", Session: rec.Session}}
	}
	fenceRouter.mu.Unlock()
	post(t, fsrv, "/v1/logs", mustMarshal(t, oneSessionLog("s_block", "block")), "application/x-protobuf", token, false)
	<-inHTTP
	fr.OnStateChange("enr_f")
	_ = fr.Stats().Snapshot()
	if code := post(t, fsrv, "/v1/logs", mustMarshal(t, oneSessionLog("s_other", "other")), "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("export during in-flight HTTP = %d, want 200", code)
	}
	close(releaseHTTP)
	waitFor(t, func() bool { return httpCalls.Load() >= 2 })

	// 4d) dropDeselected is scoped strictly to the active account's lane: Bob never discards Alice's .teams/@<alice>/ backlog.
	aliceRT := teamRoute{accountA, "enr_shared", "claude-code"}
	bobRT := teamRoute{accountB, "enr_shared", "claude-code"}
	if err := o.putTeam(aliceRT, Queued{Enrollment: "enr_shared", Project: "p", Account: accountA, Kind: "team", Session: "sa"}, newEntry(now, Logs, 1), mustMarshal(t, rawLog)); err != nil {
		t.Fatal(err)
	}
	if err := o.putTeam(bobRT, Queued{Enrollment: "enr_shared", Project: "p", Account: accountB, Kind: "team", Session: "sb"}, newEntry(now, Logs, 1), mustMarshal(t, rawLog)); err != nil {
		t.Fatal(err)
	}
	deselRouter := &stubRouter{
		active:     true,
		selectedFn: func(_, _ string) bool { return false }, // deselected under active account Bob
		recheckFn: func(_, _ string) Rechecker {
			return stubRechecker{waits: true, waitRes: Rechecked{Verdict: Wait, Why: WhyBlocked}}
		},
	}
	dr, _ := startTeamRelay(t, Options{Dir: dir, SignIn: &testSignIn{account: accountB}, Router: deselRouter})
	waitFor(t, func() bool {
		dr.mu.Lock()
		defer dr.mu.Unlock()
		return dr.teamSenders[aliceRT] != nil && dr.teamSenders[bobRT] != nil
	})
	dr.dropDeselected()
	aliceLeft, _ := o.listTeam(aliceRT)
	bobLeft, _ := o.listTeam(bobRT)
	if len(bobLeft) != 0 {
		t.Fatalf("Bob's deselected backlog has %d parts, want 0", len(bobLeft))
	}
	if len(aliceLeft) != 1 {
		t.Fatalf("Bob's dropDeselected discarded Alice's backlog (%d left, want 1)", len(aliceLeft))
	}
}

// Invariant 5: Ingest 403 ORG_NOT_ACCESSIBLE NEVER calls SignIn.Refused/Retired, triggers
// coalesced non-blocking NudgeRefresh, and OnStateChange immediately wakes the sender past
// refusedBackoff (5m) and resets backoff to 0 on recovery.
func TestIngest403OrgNotAccessibleNudgeAndImmediateWakeup(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	si := &testSignIn{account: accountA}
	var nudges atomic.Int32
	nudged := make(chan string, 4)
	router := &stubRouter{
		active: true,
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	dir := t.TempDir()
	rt := teamRoute{accountA, "enr_403", "claude-code"}
	if err := (outbox{dir: dir}).putTeam(
		rt, Queued{Enrollment: "enr_403", Project: "proj_403", Account: accountA, Kind: "team"},
		newEntry(time.Now(), Logs, 1), mustMarshal(t, oneSessionLog("s403", "hi")),
	); err != nil {
		t.Fatal(err)
	}
	r, _ := startTeamRelay(t, Options{
		Dir:    dir,
		SignIn: si,
		Router: router,
		NudgeRefresh: func(enrollment string) {
			nudges.Add(1)
			nudged <- enrollment
		},
	})

	// Wait for the 403 nudge; verify SignIn.Refused/Retired were NEVER called.
	select {
	case enr := <-nudged:
		if enr != "enr_403" {
			t.Fatalf("nudged enrollment = %q, want enr_403", enr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for NudgeRefresh on 403 ORG_NOT_ACCESSIBLE")
	}
	si.mu.Lock()
	if len(si.refused) != 0 || si.retired {
		si.mu.Unlock()
		t.Fatalf("403 ORG_NOT_ACCESSIBLE mutated sign-in: refused=%v retired=%v", si.refused, si.retired)
	}
	si.mu.Unlock()

	// Sender is now sleeping in refusedBackoff (5m). OnStateChange must wake it IMMEDIATELY and deliver!
	start := time.Now()
	r.OnStateChange("enr_403")
	waitFor(t, func() bool { return attempts.Load() == 2 })
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("OnStateChange took %v to wake sender past refusedBackoff", elapsed)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	if nudges.Load() != 1 {
		t.Fatalf("nudges = %d, want 1", nudges.Load())
	}
}

func TestReviewEdgeCases(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	t.Run("dropMixed_respects_run_boundary", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_mix", "claude-code"}
		o := outbox{dir: dir}
		e1 := newEntry(t0, Logs, 1)
		e1.q = Queued{Enrollment: "enr_mix", Project: "p", Account: accountA, Session: "s1", Run: "run-1", At: t0.Add(2 * time.Second), EndAt: t0.Add(2 * time.Second)}
		e2 := newEntry(t0.Add(time.Minute), Logs, 1)
		e2.q = Queued{Enrollment: "enr_mix", Project: "p", Account: accountA, Session: "s1", Run: "run-2", At: t0.Add(time.Minute), EndAt: t0.Add(time.Minute)}
		for _, e := range []entry{e1, e2} {
			if err := o.putTeam(rt, e.q, e, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		r := New(Options{Dir: dir})
		s := &sender{r: r, team: &rt, dir: o.teamDir(rt), name: rt.String(), queue: map[Signal][]entry{Logs: {e1, e2}}, wake: make(chan struct{}, 1), stateChange: make(chan struct{}, 1)}
		r.teamSenders[rt] = s
		r.dropMixed("s1", "run-1", t0.Add(time.Second))
		entries, _ := o.listTeam(rt)
		if len(entries) != 1 || entries[0].q.Run != "run-2" {
			t.Fatalf("entries after run-1 dropMixed = %+v, want only run-2", entries)
		}
	})

	t.Run("pinnedHold_splits_clean_pre_mixed_records", func(t *testing.T) {
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: accountA}
		mixedAt := t0.Add(15 * time.Second)
		var state atomic.Int32 // 0=Hold(pinned), 1=Mixed at mixedAt
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				return Routed{Outcome: Hold, Why: WhyPending, Queued: Queued{Enrollment: "enr_pin", Project: "proj_pin", Account: rec.Account, Kind: "team", Session: rec.Session}}
			},
			recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{entryFn: func(q Queued) Rechecked {
					if state.Load() == 0 {
						return Rechecked{Verdict: Wait, Why: WhyPending}
					}
					if !q.EffectiveEnd().Before(mixedAt) {
						return Rechecked{Verdict: Discard, Why: WhyMixed, Blocker: "mixed:s1", MixedAt: mixedAt}
					}
					return Rechecked{Verdict: SendNow}
				}}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r, srv := startTeamRelay(t, Options{
			Dir: t.TempDir(), SignIn: si, Router: router, Hold: time.Hour,
			Lookup: func(string, time.Time) (claim.Claim, bool) {
				return claim.Claim{ProjectID: "proj_pin", Kind: claim.KindRepository, Tool: "claude-code", PIDs: []int{1}}, true
			},
		})
		// Export 2 spans in one batch: clean [t0, t0+10s] and straddling [t0+10s, t0+20s].
		spans := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
				{Name: "clean", StartTimeUnixNano: uint64(t0.UnixNano()), EndTimeUnixNano: uint64(t0.Add(10 * time.Second).UnixNano())},
				{Name: "straddle", StartTimeUnixNano: uint64(t0.Add(10 * time.Second).UnixNano()), EndTimeUnixNano: uint64(t0.Add(20 * time.Second).UnixNano())},
			}}},
		}}}
		if code := post(t, srv, "/v1/traces", mustMarshal(t, spans), "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("post traces = %d", code)
		}
		r.sweep() // pins into pinnedHold
		state.Store(1)
		r.sweep() // splits pre-Mixed clean span and drops straddling span
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.traces"] == 1 && c["dropped.mixed_run.traces"] == 1 })
	})

	t.Run("corrupt_team_header_cleaned_and_counted", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_bad", "claude-code"}
		o := outbox{dir: dir}
		d := o.teamDir(rt)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		bad := newEntry(time.Now(), Logs, 3)
		if err := os.WriteFile(filepath.Join(d, bad.name), []byte("{not-json\nbody"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := New(Options{Dir: dir})
		r.janitor(time.Now())
		if _, err := os.Stat(filepath.Join(d, bad.name)); !os.IsNotExist(err) {
			t.Fatalf("corrupt file still exists: err=%v", err)
		}
		if got := r.Stats().Snapshot().Counters["dropped.outbox_unreadable.logs"]; got != 3 {
			t.Fatalf("dropped.outbox_unreadable.logs = %d, want 3", got)
		}
	})

	t.Run("blocked_nudge_does_not_hang_shutdown", func(t *testing.T) {
		t.Parallel()
		unblock := make(chan struct{})
		defer close(unblock)
		started := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		r := New(Options{
			Dir: t.TempDir(), Grace: 50 * time.Millisecond,
			NudgeRefresh: func(string) {
				started <- struct{}{}
				<-unblock
			},
		})
		done := make(chan struct{})
		go func() { r.Run(ctx); close(done) }()
		r.nudgeRefresh("enr_slow")
		<-started
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Relay.Run hung on blocked NudgeRefresh callback")
		}
	})

	t.Run("fence_aborts_on_zero_to_one_generation_transition", func(t *testing.T) {
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: accountA}
		var gen atomic.Int64 // starts at 0
		var includePrompts atomic.Bool
		includePrompts.Store(true)
		var resolveCalls atomic.Int32
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_g0", Project: "proj_g0", Account: rec.Account, Session: rec.Session}}
			},
			recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{gen: gen.Load(), entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				if resolveCalls.Add(1) == 2 { // first sender batch resolve: bump gen 0 -> 1 before fence
					includePrompts.Store(false)
					gen.Store(1)
				}
				return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: includePrompts.Load(), IncludeToolContent: true}, nil
			},
		}
		r, srv := startTeamRelay(t, Options{
			Dir: t.TempDir(), SignIn: si, Router: router,
			Lookup: func(string, time.Time) (claim.Claim, bool) {
				return claim.Claim{ProjectID: "proj_g0", Kind: claim.KindRepository, Tool: "claude-code", PIDs: []int{1}}, true
			},
		})
		logs := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				TimeUnixNano: uint64(time.Now().UnixNano()),
				Attributes:   []*commonpb.KeyValue{kv("event.name", "user_prompt"), kv("prompt", "secret-prompt")},
			}}}},
		}}}
		if code := post(t, srv, "/v1/logs", mustMarshal(t, logs), "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("post logs = %d", code)
		}
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
		for _, body := range up.bodies() {
			if bytes.Contains(body, []byte("secret-prompt")) {
				t.Fatal("fence allowed gen 0->1 batch through with stale prompt policy")
			}
		}
	})

	t.Run("team_waits_discard_drops_queued_in_nextBatch_and_fence", func(t *testing.T) {
		t.Parallel()
		for _, inFence := range []bool{false, true} {
			name := "nextBatch"
			if inFence {
				name = "fence"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				up := newSignInUpstream(t)
				si := &testSignIn{account: accountA}
				dir := t.TempDir()
				rt := teamRoute{accountA, "enr_tw", "claude-code"}
				o := outbox{dir: dir}
				e := newEntry(time.Now(), Logs, 2)
				if err := o.putTeam(rt, Queued{Enrollment: "enr_tw", Project: "proj_tw", Account: accountA, Session: "s1"}, e, mustMarshal(t, oneSessionLog("s1", "hi"))); err != nil {
					t.Fatal(err)
				}
				var discardNow atomic.Bool
				if !inFence {
					discardNow.Store(true)
				}
				router := &stubRouter{
					active: true,
					recheckFn: func(_, _ string) Rechecker {
						if discardNow.Load() {
							return stubRechecker{gen: 1, waits: true, waitRes: Rechecked{Verdict: Discard, Why: WhyDeselected}}
						}
						return stubRechecker{gen: 1, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
					},
					resolveFn: func(_, _, account string) (Policy, error) {
						if inFence {
							discardNow.Store(true)
						}
						return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
					},
				}
				r, _ := startTeamRelay(t, Options{Dir: dir, SignIn: si, Router: router})
				waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.team_deselected.logs"] == 2 })
				if len(up.bodies()) != 0 {
					t.Fatalf("expected 0 upstream posts when TeamWaits returned Discard, got %d", len(up.bodies()))
				}
				if entries, err := o.listTeam(rt); err != nil || len(entries) != 0 {
					t.Fatalf("expected empty outbox after TeamWaits Discard, got entries=%d err=%v", len(entries), err)
				}
			})
		}
	})

	t.Run("transient_unreadable_team_file_fails_queued_sessions_and_recovers", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("POSIX chmod 0000 test")
		}
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: accountA}
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_perm", "claude-code"}
		o := outbox{dir: dir}
		e := newEntry(time.Now(), Logs, 1)
		if err := o.putTeam(rt, Queued{Enrollment: "enr_perm", Project: "proj_perm", Account: accountA, Session: "sess-unreadable"}, e, mustMarshal(t, oneSessionLog("sess-unreadable", "hello"))); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(o.teamDir(rt), e.name)
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

		if _, err := QueuedSessions(dir); err == nil {
			t.Fatal("QueuedSessions returned nil error for unreadable team file")
		}
		router := &stubRouter{
			active: true,
			recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{gen: 1, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r, _ := startTeamRelay(t, Options{Dir: dir, SignIn: si, Router: router})
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		r.OnStateChange("enr_perm")
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	})

	t.Run("server_key_lane_switch_blocked_by_pre_post_fence", func(t *testing.T) {
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: sendas.NoPersonLane} // starts in no_person lane
		dir := t.TempDir()
		rt := teamRoute{sendas.NoPersonLane, "enr_sk", "claude-code"}
		o := outbox{dir: dir}
		if err := o.putTeam(rt, Queued{Enrollment: "enr_sk", Project: "proj_sk", Account: sendas.NoPersonLane, Session: "s_sk"}, newEntry(time.Now(), Logs, 1), mustMarshal(t, oneSessionLog("s_sk", "server-key"))); err != nil {
			t.Fatal(err)
		}
		var switched atomic.Bool
		router := &stubRouter{
			active: true,
			recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{gen: 1, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				if switched.CompareAndSwap(false, true) {
					si.set(accountA) // user signs in after batch selection, before HTTP.Do
				}
				return Policy{Endpoint: up.srv.URL, Key: "srv-key-fence", Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r, _ := startTeamRelay(t, Options{Dir: dir, SignIn: si, Router: router})
		waitFor(t, func() bool {
			if !switched.Load() {
				return false
			}
			r.mu.Lock()
			s := r.teamSenders[rt]
			r.mu.Unlock()
			return s != nil && !s.busy()
		})
		if got := len(up.bodies()); got != 0 {
			t.Fatalf("expected 0 HTTP posts after no_person -> user lane switch, got %d", got)
		}
		si.set(sendas.NoPersonLane)
		r.OnStateChange("enr_sk")
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	})

	t.Run("bounded_candidate_scan_delivers_tail_without_starvation", func(t *testing.T) {
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: accountA}
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_scan", "claude-code"}
		o := outbox{dir: dir}
		t0 := time.Now().Add(-time.Minute)
		const waitCount = 600 // > 2 scan windows (maxMergeFiles*4 = 256)
		for i := range waitCount {
			if err := o.putTeam(rt, Queued{Enrollment: "enr_scan", Project: "proj_scan", Account: accountA, Session: "wait"}, newEntry(t0.Add(time.Duration(i)*time.Millisecond), Logs, 1), mustMarshal(t, oneSessionLog("wait", "w"))); err != nil {
				t.Fatal(err)
			}
		}
		if err := o.putTeam(rt, Queued{Enrollment: "enr_scan", Project: "proj_scan", Account: accountA, Session: "go"}, newEntry(t0.Add(waitCount*time.Millisecond), Logs, 1), mustMarshal(t, oneSessionLog("go", "tail"))); err != nil {
			t.Fatal(err)
		}
		var maxPerCall atomic.Int32
		router := &stubRouter{
			active: true,
			recheckFn: func(_, _ string) Rechecker {
				var calls atomic.Int32
				return stubRechecker{gen: 1, entryFn: func(q Queued) Rechecked {
					n := calls.Add(1)
					for {
						cur := maxPerCall.Load()
						if n <= cur || maxPerCall.CompareAndSwap(cur, n) {
							break
						}
					}
					if q.Session == "go" {
						return Rechecked{Verdict: SendNow}
					}
					return Rechecked{Verdict: Wait, Why: WhyPending}
				}}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r, _ := startTeamRelay(t, Options{Dir: dir, SignIn: si, Router: router})
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
		if got := maxPerCall.Load(); got > int32(maxMergeFiles*4) {
			t.Fatalf("single Recheck scanned %d entries, want <= %d", got, maxMergeFiles*4)
		}
		waitFor(t, func() bool {
			r.mu.Lock()
			s := r.teamSenders[rt]
			r.mu.Unlock()
			return s != nil && !s.busy()
		})
		// Entry-level Wait must leave s.blocked=false so a newly pushed SendNow part wakes the sender immediately.
		r.enqueueTeam(Queued{Enrollment: "enr_scan", Project: "proj_scan", Account: accountA, Session: "go"}, "claude-code", &part{
			signal: Logs, records: 1, account: accountA, arrived: time.Now(), msg: oneSessionLog("go", "pushed"),
		})
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 2 })
	})

	t.Run("process_attributed_metrics_split_across_moved_placements", func(t *testing.T) {
		t.Parallel()
		now := time.Now().UTC().Truncate(time.Second)
		t2 := now.Add(-2 * time.Second)
		t1 := t2.Add(-time.Minute)
		claims := map[string]claim.Claim{
			"s_proc": {
				PIDs: []int{4242},
				Placements: []claim.Placement{
					{Since: t1.Add(-time.Second), Kind: claim.KindRepository, ProjectID: "proj_1", Tool: "codex", PIDs: []int{4242}},
					{Since: t2, Kind: claim.KindRepository, ProjectID: "proj_2", Tool: "codex", PIDs: []int{4242}},
				},
			},
		}
		up1, up2 := newSignInUpstream(t), newSignInUpstream(t)
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				if !rec.Placed {
					return Routed{Outcome: Hold, Why: WhyUnknown}
				}
				enr := "enr_1"
				if rec.Placement.ProjectID == "proj_2" {
					enr = "enr_2"
				}
				return Routed{Outcome: Send, Queued: Queued{Enrollment: enr, Project: rec.Placement.ProjectID, Account: rec.Account, Kind: "team", Session: rec.Session}}
			},
			resolveFn: func(enr, _, account string) (Policy, error) {
				ep := up1.srv.URL
				if enr == "enr_2" {
					ep = up2.srv.URL
				}
				return Policy{Endpoint: ep, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r := newRelay(Options{
			Dir: t.TempDir(), Now: func() time.Time { return now }, Hold: time.Hour, TraceHold: time.Hour,
			SignIn: &testSignIn{account: accountA}, Router: router,
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) { c, ok := claims[session]; return c, ok },
		})
		runRelay(t, r)
		r.learnProcess(4242, "s_proc")
		r.mu.Lock()
		r.procs[4242].exitedAt = now.Add(-exitGrace - time.Second)
		r.mu.Unlock()
		// Sessionless metrics batch with one point before the move (t1) and one after (t2+1s).
		md := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: "m",
				Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
					{StartTimeUnixNano: uint64(t1.UnixNano()), TimeUnixNano: uint64(t1.UnixNano())},
					{StartTimeUnixNano: uint64(t2.Add(time.Second).UnixNano()), TimeUnixNano: uint64(t2.Add(time.Second).UnixNano())},
				}}},
			}}}},
		}}}
		p := &part{signal: Metrics, msg: md, records: 2, pid: 4242, session: procKey(4242), account: accountA, arrived: now}
		p.at = earliest(p.msg)
		p.startAt, p.endAt = intervalOf(p.msg)
		r.route(p)
		waitFor(t, func() bool { return len(up1.requests()) == 1 && len(up2.requests()) == 1 })
		if got, want := up1.requests()[0], "Bearer token-"+accountA+" proj_1"; got != want {
			t.Fatalf("pre-move metric routed to %q, want %q", got, want)
		}
		if got, want := up2.requests()[0], "Bearer token-"+accountA+" proj_2"; got != want {
			t.Fatalf("post-move metric routed to %q, want %q", got, want)
		}
	})

	t.Run("startup_team_routes_discovery_failure_retried_by_janitor", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("POSIX chmod 0000 test")
		}
		t.Parallel()
		up := newSignInUpstream(t)
		si := &testSignIn{account: accountA}
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_disc", "claude-code"}
		o := outbox{dir: dir}
		if err := o.putTeam(rt, Queued{Enrollment: "enr_disc", Project: "proj_disc", Account: accountA, Session: "s_disc"}, newEntry(time.Now(), Logs, 1), mustMarshal(t, oneSessionLog("s_disc", "retry-discover"))); err != nil {
			t.Fatal(err)
		}
		acctDir := filepath.Join(dir, teamsDir, accountPrefix+accountA)
		if err := os.Chmod(acctDir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(acctDir, 0o700) })

		router := &stubRouter{
			active: true,
			recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{gen: 1, entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				return Policy{Endpoint: up.srv.URL, Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r, _ := startTeamRelay(t, Options{Dir: dir, SignIn: si, Router: router})
		waitFor(t, func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.teamDiscoverErr
		})
		if err := os.Chmod(acctDir, 0o700); err != nil {
			t.Fatal(err)
		}
		r.janitor(time.Now())
		waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
		r.mu.Lock()
		stillErr := r.teamDiscoverErr
		r.mu.Unlock()
		if stillErr {
			t.Fatal("expected teamDiscoverErr=false after janitor retry succeeded")
		}
	})

	t.Run("sessionless_record_from_wholly_uncollected_process_dropped_not_personal", func(t *testing.T) {
		t.Parallel()
		now := t0.Add(time.Minute)
		var routeCalls atomic.Int32
		router := &stubRouter{
			active: true, unknownPers: true,
			routeFn: func(rec Record) Routed {
				routeCalls.Add(1)
				return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_personal", Project: "proj_personal", Account: rec.Account, Kind: "unknown"}}
			},
		}
		r := New(Options{
			Dir: t.TempDir(), Router: router, SignIn: &testSignIn{account: accountA},
			Now:          func() time.Time { return now },
			ProcessAlive: func(int) bool { return true },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if session == "s_nc1" || session == "s_nc2" {
					return claim.Claim{PIDs: []int{5151}, Tool: "claude-code"}, true
				}
				return claim.Claim{}, false
			},
		})
		r.learnProcess(5151, "s_nc1")
		r.learnProcess(5151, "s_nc2")
		p := &part{signal: Logs, msg: oneSessionLog("", "anon"), records: 1, pid: 5151, session: procKey(5151), account: accountA, arrived: now, at: now, endAt: now}
		r.route(p)
		if routeCalls.Load() != 0 {
			t.Fatalf("Router.Route called %d times for wholly uncollected process, want 0", routeCalls.Load())
		}
		if got := r.Stats().Snapshot().Counters["dropped.not_collected.logs"]; got != 1 {
			t.Fatalf("dropped.not_collected.logs = %d, want 1", got)
		}
	})

	t.Run("routed_placement_without_project_id_not_treated_as_uncollected", func(t *testing.T) {
		t.Parallel()
		now := t0.Add(time.Minute)
		r := New(Options{
			Dir: t.TempDir(), Now: func() time.Time { return now },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if session == "s_repo" {
					return claim.Claim{Kind: claim.KindRepository, PIDs: []int{5252}, Tool: "claude-code"}, true
				}
				return claim.Claim{}, false
			},
		})
		r.learnProcess(5252, "s_repo")
		if r.collectsNone(5252, now) {
			t.Fatal("collectsNone returned true for routed placement with non-empty Kind")
		}
	})

	t.Run("interval_ending_in_not_collected_placement_is_dropped", func(t *testing.T) {
		t.Parallel()
		var routeCalls atomic.Int32
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				routeCalls.Add(1)
				return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_repo", Project: "proj_repo", Account: rec.Account, Kind: "repository"}}
			},
		}
		movedAt := t0.Add(10 * time.Second)
		r := New(Options{
			Dir: t.TempDir(), Router: router, SignIn: &testSignIn{account: accountA},
			Now: func() time.Time { return t0.Add(time.Minute) },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if session != "s_end_nc" {
					return claim.Claim{}, false
				}
				return claim.Claim{
					Tool: "claude-code",
					Placements: []claim.Placement{
						{Since: t0, Kind: claim.KindRepository, PIDs: []int{6161}, Remote: "github.com/acme/repo"},
						{Since: movedAt, PIDs: []int{6161}}, // explicit not-collected mark
					},
				}, true
			},
		})
		spans := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s_end_nc")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
				Name:              "straddle-nc",
				StartTimeUnixNano: uint64(t0.Add(5 * time.Second).UnixNano()),
				EndTimeUnixNano:   uint64(t0.Add(15 * time.Second).UnixNano()),
			}}}},
		}}}
		p := &part{signal: Traces, msg: spans, records: 1, pid: 6161, session: "s_end_nc", account: accountA, arrived: t0.Add(time.Minute)}
		p.at = earliest(p.msg)
		p.startAt, p.endAt = intervalOf(p.msg)
		r.route(p)
		if routeCalls.Load() != 0 {
			t.Fatalf("Router.Route called %d times for span ending in not-collected placement, want 0", routeCalls.Load())
		}
		if got := r.Stats().Snapshot().Counters["dropped.not_collected.traces"]; got != 1 {
			t.Fatalf("dropped.not_collected.traces = %d, want 1", got)
		}
	})

	t.Run("interval_crossing_collected_placements_is_dropped", func(t *testing.T) {
		t.Parallel()
		var routeCalls atomic.Int32
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				routeCalls.Add(1)
				return Routed{Outcome: Send, Queued: Queued{Enrollment: "enr_a", Project: "proj_a", Account: rec.Account, Kind: "repository"}}
			},
		}
		movedAt := t0.Add(10 * time.Second)
		r := New(Options{
			Dir: t.TempDir(), Router: router, SignIn: &testSignIn{account: accountA},
			Now: func() time.Time { return t0.Add(time.Minute) },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if session != "s_cross_ab" {
					return claim.Claim{}, false
				}
				return claim.Claim{
					Tool: "claude-code",
					Placements: []claim.Placement{
						{Since: t0, Kind: claim.KindRepository, PIDs: []int{7171}, Repository: config.Repository{Origin: "github.com/acme/a"}, Run: "r1"},
						{Since: movedAt, Kind: claim.KindRepository, PIDs: []int{7171}, Repository: config.Repository{Origin: "github.com/beta/b"}, Run: "r2"},
					},
				}, true
			},
		})
		spans := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s_cross_ab")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
				Name:              "straddle-ab",
				StartTimeUnixNano: uint64(t0.Add(5 * time.Second).UnixNano()),
				EndTimeUnixNano:   uint64(t0.Add(15 * time.Second).UnixNano()),
			}}}},
		}}}
		p := &part{signal: Traces, msg: spans, records: 1, pid: 7171, session: "s_cross_ab", account: accountA, arrived: t0.Add(time.Minute)}
		p.at = earliest(p.msg)
		p.startAt, p.endAt = intervalOf(p.msg)
		r.route(p)
		if routeCalls.Load() != 0 {
			t.Fatalf("Router.Route called %d times for span crossing A->B, want 0", routeCalls.Load())
		}
		if got := r.Stats().Snapshot().Counters["dropped.mixed_run.traces"]; got != 1 {
			t.Fatalf("dropped.mixed_run.traces = %d, want 1", got)
		}
	})

	t.Run("batch_with_A_B_A_isolates_straddler_and_delivers_clean_segments", func(t *testing.T) {
		t.Parallel()
		var routedOrigins []string
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				routedOrigins = append(routedOrigins, rec.Placement.Repository.Origin)
				enr := "enr_a"
				if rec.Placement.Repository.Origin == "github.com/beta/b" {
					enr = "enr_b"
				}
				return Routed{Outcome: Send, Queued: Queued{Enrollment: enr, Project: "p_" + enr, Account: rec.Account, Kind: "repository"}}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				return Policy{Endpoint: "https://example.invalid", Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r := New(Options{
			Dir: t.TempDir(), Router: router, SignIn: &testSignIn{account: accountA},
			Now: func() time.Time { return t0.Add(time.Minute) },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				if session != "s_aba" {
					return claim.Claim{}, false
				}
				return claim.Claim{
					Tool: "claude-code",
					Placements: []claim.Placement{
						{Since: t0, Kind: claim.KindRepository, PIDs: []int{8181}, Repository: config.Repository{Origin: "github.com/acme/a"}, Run: "r1"},
						{Since: t0.Add(10 * time.Second), Kind: claim.KindRepository, PIDs: []int{8181}, Repository: config.Repository{Origin: "github.com/beta/b"}, Run: "r2"},
						{Since: t0.Add(20 * time.Second), Kind: claim.KindRepository, PIDs: []int{8181}, Repository: config.Repository{Origin: "github.com/acme/a"}, Run: "r1"},
					},
				}, true
			},
		})
		// The senders route starts write to the outbox: stop them before its folder goes.
		t.Cleanup(func() { r.cancelSend(); r.wg.Wait() })
		spans := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s_aba")}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
				{Name: "clean-a1", StartTimeUnixNano: uint64(t0.Add(1 * time.Second).UnixNano()), EndTimeUnixNano: uint64(t0.Add(2 * time.Second).UnixNano())},
				{Name: "clean-b", StartTimeUnixNano: uint64(t0.Add(12 * time.Second).UnixNano()), EndTimeUnixNano: uint64(t0.Add(13 * time.Second).UnixNano())},
				{Name: "clean-a2", StartTimeUnixNano: uint64(t0.Add(25 * time.Second).UnixNano()), EndTimeUnixNano: uint64(t0.Add(26 * time.Second).UnixNano())},
				{Name: "straddle-aba", StartTimeUnixNano: uint64(t0.Add(1 * time.Second).UnixNano()), EndTimeUnixNano: uint64(t0.Add(26 * time.Second).UnixNano())},
			}}},
		}}}
		p := &part{signal: Traces, msg: spans, records: 4, pid: 8181, session: "s_aba", account: accountA, arrived: t0.Add(time.Minute)}
		p.at = earliest(p.msg)
		p.startAt, p.endAt = intervalOf(p.msg)
		r.route(p)
		if len(routedOrigins) != 3 {
			t.Fatalf("routed %d clean segments (%v), want 3", len(routedOrigins), routedOrigins)
		}
		if got := r.Stats().Snapshot().Counters["dropped.mixed_run.traces"]; got != 1 {
			t.Fatalf("dropped.mixed_run.traces = %d, want 1", got)
		}
	})

	t.Run("partial_listTeam_error_retries_recovery_without_duplicating", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("requires POSIX non-root chmod 000")
		}
		dir := t.TempDir()
		rt := teamRoute{accountA, "enr_retry", "claude-code"}
		o := outbox{dir: dir}
		e1 := newEntry(t0, Logs, 1)
		e2 := newEntry(t0.Add(time.Second), Logs, 1)
		q := Queued{Enrollment: "enr_retry", Project: "p", Account: accountA, Session: "s1"}
		if err := o.putTeam(rt, q, e1, []byte("b1")); err != nil {
			t.Fatal(err)
		}
		if err := o.putTeam(rt, q, e2, []byte("b2")); err != nil {
			t.Fatal(err)
		}
		p2 := filepath.Join(o.teamDir(rt), e2.name)
		if err := os.Chmod(p2, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p2, 0o600) })

		r := New(Options{
			Dir: dir, SignIn: &testSignIn{account: accountA},
			Router: &stubRouter{active: true, recheckFn: func(_, _ string) Rechecker {
				return stubRechecker{waits: true, waitRes: Rechecked{Verdict: Wait, Why: WhyBlocked}}
			}},
		})
		t.Cleanup(func() { r.cancelSend(); r.wg.Wait() })
		r.recoverTeamOutbox()
		r.mu.Lock()
		discoverErr := r.teamDiscoverErr
		recovered := r.teamRecovered[rt]
		r.mu.Unlock()
		if !discoverErr || recovered {
			t.Fatalf("after partial listTeam error: teamDiscoverErr=%v teamRecovered=%v, want true, false", discoverErr, recovered)
		}
		if err := os.Chmod(p2, 0o600); err != nil {
			t.Fatal(err)
		}
		r.recoverTeamOutbox()
		r.mu.Lock()
		discoverErr = r.teamDiscoverErr
		recovered = r.teamRecovered[rt]
		s := r.teamSenders[rt]
		r.mu.Unlock()
		if discoverErr || !recovered {
			t.Fatalf("after chmod restore: teamDiscoverErr=%v teamRecovered=%v, want false, true", discoverErr, recovered)
		}
		s.mu.Lock()
		got := len(s.queue[Logs])
		s.mu.Unlock()
		if got != 2 {
			t.Fatalf("queued entries after retry = %d, want 2 (no duplicates)", got)
		}
	})

	t.Run("sweepPinned_memoizes_teamReady_per_pass", func(t *testing.T) {
		t.Parallel()
		var resolveCalls atomic.Int32
		var verdict atomic.Int32
		verdict.Store(int32(Wait))
		router := &stubRouter{
			active: true,
			routeFn: func(rec Record) Routed {
				return Routed{
					Outcome: Hold,
					Why:     WhyBlocked,
					Queued:  Queued{Enrollment: "enr_memo", Project: "p_memo", Account: rec.Account, Kind: "repository"},
				}
			},
			recheckFn: func(_, _ string) Rechecker {
				if Verdict(verdict.Load()) == Wait {
					return stubRechecker{waits: true, waitRes: Rechecked{Verdict: Wait, Why: WhyBlocked}}
				}
				return stubRechecker{entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} }}
			},
			resolveFn: func(_, _, account string) (Policy, error) {
				resolveCalls.Add(1)
				return Policy{Endpoint: "https://example.invalid", Account: account, IncludePrompts: true, IncludeToolContent: true}, nil
			},
		}
		r := New(Options{
			Router: router,
			SignIn: &testSignIn{account: accountA},
			Now:    func() time.Time { return t0 },
			Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
				return claim.Claim{Tool: "claude-code", Kind: claim.KindRepository, Placements: []claim.Placement{{Since: t0.Add(-time.Minute), Kind: claim.KindRepository}}}, true
			},
		})
		t.Cleanup(func() { r.cancelSend(); r.wg.Wait() })

		const pinnedCount = 50
		for i := range pinnedCount {
			p := &part{
				signal:  Logs,
				msg:     oneSessionLog("s_memo", fmt.Sprintf("msg-%d", i)),
				records: 1,
				session: "s_memo",
				account: accountA,
				arrived: t0,
			}
			p.at = earliest(p.msg)
			p.startAt, p.endAt = intervalOf(p.msg)
			r.route(p)
		}
		if r.heldN != pinnedCount {
			t.Fatalf("heldN = %d, want %d", r.heldN, pinnedCount)
		}
		verdict.Store(int32(SendNow))
		r.sweep()
		if r.heldN != 0 {
			t.Fatalf("heldN after release = %d, want 0", r.heldN)
		}
		if got := r.Stats().Snapshot().Counters["released_after_hold"]; got != pinnedCount {
			t.Fatalf("released_after_hold = %d, want %d", got, pinnedCount)
		}
		if got := resolveCalls.Load(); got != 1 {
			t.Fatalf("Router.Resolve called %d times for %d pinned parts in one sweep, want 1", got, pinnedCount)
		}
	})
}

func oneSessionLog(session, body string) *logspb.LogsData {
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", session)}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: uint64(time.Now().UnixNano()),
			Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		}}}},
	}}}
}

func (u *signInUpstream) bodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([][]byte, len(u.sent))
	for i, b := range u.sent {
		out[i] = bytes.Clone(b)
	}
	return out
}

// On the routed relay path, cumulative metrics (Sum, Histogram, ExponentialHistogram
// with CUMULATIVE temporality, plus Summary and Gauge) whose StartTimeUnixNano is process
// start and whose TimeUnixNano is after a placement move must be placed by TimeUnixNano
// rather than dropped as mixed_run, while DELTA metrics spanning a move still drop.
func TestRoutedRelayPlacesCumulativeMetricsByReadTimeNotProcessStart(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	movedAt := t0.Add(time.Minute)
	after := movedAt.Add(5 * time.Second)
	ns := func(tm time.Time) uint64 { return uint64(tm.UnixNano()) }

	u := newSignInUpstream(t)
	si := &testSignIn{account: accountA}
	var routedOrigins []string
	var routedMu sync.Mutex
	router := &stubRouter{
		active: true,
		routeFn: func(rec Record) Routed {
			routedMu.Lock()
			routedOrigins = append(routedOrigins, rec.Placement.Repository.Origin)
			routedMu.Unlock()
			return Routed{
				Outcome: Send,
				Queued: Queued{
					Enrollment: "enr_acme", Project: "proj_acme", Account: accountA,
					Kind: "repository", Repository: rec.Placement.Repository.Origin,
					Session: rec.Session, Run: rec.Placement.Run, At: rec.At, EndAt: rec.EndAt,
				},
			}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}

	claims := map[string]claim.Claim{
		"s_move": {Tool: "gemini-cli", Placements: []claim.Placement{
			{Since: t0.Add(-time.Hour), Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/acme/a"}, Run: "r1"},
			{Since: movedAt, Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/acme/b"}, Run: "r1"},
		}},
		"s_to_nc": {Tool: "gemini-cli", Placements: []claim.Placement{
			{Since: t0.Add(-time.Hour), Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/acme/a"}, Run: "r1"},
			{Since: movedAt}, // moved to not-collected mark
		}},
	}
	r, srv := startTeamRelay(t, Options{
		SignIn: si, Router: router,
		Lookup: func(session string, _ time.Time) (claim.Claim, bool) {
			c, ok := claims[session]
			return c, ok
		},
	})

	cum := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	delta := metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA

	// Batch for s_move contains all 5 cumulative/read-time metric kinds (all with
	// StartTimeUnixNano = t0 before movedAt, TimeUnixNano = after) plus 1 DELTA
	// point straddling movedAt. The 5 read-time points must forward to github.com/acme/b;
	// only the 1 DELTA straddler must drop as mixed_run.
	moveBatch := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s_move")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cum_sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: cum,
				DataPoints:             []*metricspb.NumberDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
			{Name: "cum_hist", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
				AggregationTemporality: cum,
				DataPoints:             []*metricspb.HistogramDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
			{Name: "cum_exp", Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{
				AggregationTemporality: cum,
				DataPoints:             []*metricspb.ExponentialHistogramDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
			{Name: "summary", Data: &metricspb.Metric_Summary{Summary: &metricspb.Summary{
				DataPoints: []*metricspb.SummaryDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
			{Name: "gauge", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
				DataPoints: []*metricspb.NumberDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
			{Name: "delta_straddle", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: delta,
				DataPoints:             []*metricspb.NumberDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
		}}},
	}}}
	if code := post(t, srv, "/v1/metrics", mustMarshal(t, moveBatch), "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("post moveBatch = %d, want 200", code)
	}

	// Cumulative metric read after moving to a not-collected mark must drop as not_collected.
	ncBatch := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s_to_nc")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "cum_sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
				AggregationTemporality: cum,
				DataPoints:             []*metricspb.NumberDataPoint{{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(after)}},
			}}},
		}}},
	}}}
	if code := post(t, srv, "/v1/metrics", mustMarshal(t, ncBatch), "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("post ncBatch = %d, want 200", code)
	}

	waitFor(t, func() bool {
		c := r.Stats().Snapshot().Counters
		return c["forwarded.metrics"] == 5 && c["dropped.mixed_run.metrics"] == 1 && c["dropped.not_collected.metrics"] == 1
	})
	routedMu.Lock()
	defer routedMu.Unlock()
	if len(routedOrigins) != 1 || routedOrigins[0] != "github.com/acme/b" {
		t.Fatalf("routed origins = %v, want [github.com/acme/b]", routedOrigins)
	}
}

// Pre-POST fence must abort and invalidate cached policy when StateID changes even if
// Generation stays at 1 (as happens after routing.ReplaceCorrupt).
func TestPrePOSTFenceAbortsOnStateIDChange(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	si := &testSignIn{account: accountA}
	var stateID atomic.Value
	stateID.Store("state-1")
	var resolveCalls atomic.Int32
	router := &stubRouter{
		active: true,
		routeFn: func(rec Record) Routed {
			return Routed{
				Outcome: Send,
				Queued: Queued{
					Enrollment: "enr_1", Project: "proj_1", Account: accountA,
					Kind: "repository", Session: rec.Session, At: rec.At, EndAt: rec.EndAt,
				},
			}
		},
		recheckFn: func(_, _ string) Rechecker {
			return stubRechecker{
				stateID: stateID.Load().(string),
				gen:     1,
				entryFn: func(Queued) Rechecked { return Rechecked{Verdict: SendNow} },
			}
		},
		resolveFn: func(_, _, account string) (Policy, error) {
			if resolveCalls.Add(1) == 2 {
				// Simulate ReplaceCorrupt: fresh StateID, Generation still 1.
				stateID.Store("state-2")
			}
			return Policy{Endpoint: u.srv.URL, Account: account, User: "usr_a", UserAccount: account, IncludePrompts: true, IncludeToolContent: true}, nil
		},
	}
	_, srv := startTeamRelay(t, Options{
		SignIn: si, Router: router,
		Lookup: func(string, time.Time) (claim.Claim, bool) {
			return claim.Claim{Kind: claim.KindRepository, ProjectID: "proj_1"}, true
		},
	})
	if code := post(t, srv, "/v1/logs", mustMarshal(t, oneSessionLog("s_stateid", "hello")), "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("post logs = %d, want 200", code)
	}
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := resolveCalls.Load(); got < 3 {
		t.Fatalf("Resolve called %d times, want >= 3 (initial + aborted pre-POST + re-resolved after StateID fence)", got)
	}
}
