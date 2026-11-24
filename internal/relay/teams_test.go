package relay

import (
	"bytes"
	"context"
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
	gen     int64
	waits   bool
	waitRes Rechecked
	entryFn func(Queued) Rechecked
}

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

	// Metrics: all 5 metric types (Sum, Gauge, Histogram, ExponentialHistogram, Summary).
	metrics := &metricspb.MetricsData{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("session.id", "s1")}},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{
			{Name: "sum", Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{DataPoints: []*metricspb.NumberDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
			{Name: "gauge", Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: []*metricspb.NumberDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(mixedAt)},
			}}}},
			{Name: "hist", Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{DataPoints: []*metricspb.HistogramDataPoint{
				{StartTimeUnixNano: ns(t0), TimeUnixNano: ns(t1)},
				{StartTimeUnixNano: ns(t1), TimeUnixNano: ns(t3)},
			}}}},
			{Name: "exp", Data: &metricspb.Metric_ExponentialHistogram{ExponentialHistogram: &metricspb.ExponentialHistogram{DataPoints: []*metricspb.ExponentialHistogramDataPoint{
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
