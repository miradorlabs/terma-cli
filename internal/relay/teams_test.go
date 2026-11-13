package relay

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// fakeRouter routes by placement: a placement's ProjectID names the enrollment it goes to
// ("e-<project>"), Kind mixed drops it, and none holds it unless unknown records may go to
// personal. Each enrollment's key and content come from policies.
type fakeRouter struct {
	mu                sync.Mutex
	migrated          bool
	unknownToPersonal bool
	policies          map[string]Policy
	deselected        map[string]bool
	waiting           map[string]bool // enrollments whose entries wait at recheck
	discard           map[string]bool // enrollments whose entries are dropped at recheck
	legacy            map[string]Queued
	routed            []Record
	rechecked         []Queued
	hold              *Routed // set: every placed record is held so
}

func (f *fakeRouter) Migrated() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.migrated }
func (f *fakeRouter) UnknownToPersonal() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unknownToPersonal
}

func (f *fakeRouter) Route(rec Record) Routed {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routed = append(f.routed, rec)
	switch {
	case rec.Placed && rec.Placement.Kind == claim.KindMixed:
		return Routed{Outcome: Drop, Why: WhyMixed, Blocker: "mixed:" + rec.Session, MixedAt: rec.At}
	case rec.Placed && f.hold != nil:
		return *f.hold
	case rec.Placed:
		return Routed{Outcome: Send, Queued: Queued{Enrollment: "e-" + rec.Placement.ProjectID, Project: rec.Placement.ProjectID,
			Kind: "repository", Repository: rec.Placement.Repository.Origin, Session: rec.Session, At: rec.At}}
	case !rec.Narrow && !rec.Claimed && f.unknownToPersonal:
		return Routed{Outcome: Send, Queued: Queued{Enrollment: "e-personal", Project: "personal", Kind: "unknown", Session: rec.Session, At: rec.At}}
	}
	return Routed{Outcome: Hold, Why: WhyUnknown}
}

func (f *fakeRouter) Recheck(q Queued) Rechecked {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rechecked = append(f.rechecked, q)
	switch {
	case f.discard[q.Enrollment]:
		return Rechecked{Verdict: Discard, Why: WhyBlocked, Blocker: "team:" + q.Enrollment}
	case f.waiting[q.Enrollment]:
		return Rechecked{Verdict: Wait, Why: WhyBlocked}
	}
	return Rechecked{Verdict: SendNow}
}

func (f *fakeRouter) Resolve(enrollment, _ string) (Policy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[enrollment]
	if !ok {
		return Policy{}, ErrNoKey
	}
	return p, nil
}

func (f *fakeRouter) Selected(enrollment string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.deselected[enrollment]
}

func (f *fakeRouter) Legacy(project string, _ config.Repository) (Queued, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, ok := f.legacy[project]
	return q, ok
}

func (f *fakeRouter) set(fn func(f *fakeRouter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRouter) records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routed)
}

// teamFixture is a migrated machine: sessions A (acme/api) and B (clientco/app) placed,
// M mixed, U unclaimed; acme's and clientco's enrollments have keys, personal's too.
type teamFixture struct {
	*fixture
	router *fakeRouter
	dir    string
}

func newTeamFixture(t *testing.T, u *upstream) *teamFixture {
	f := &teamFixture{fixture: newFixture(), dir: t.TempDir()}
	place := func(project, origin string) claim.Claim {
		return claim.Claim{ProjectID: project, Kind: claim.KindRepository, Repository: config.Repository{Origin: origin}, Tool: "claude-code"}
	}
	f.claims = map[string]claim.Claim{
		"A": place("acme", "github.com/acme/api"),
		"B": place("clientco", "github.com/clientco/app"),
		"M": {Kind: claim.KindMixed, Placements: []claim.Placement{{Kind: claim.KindMixed}}},
	}
	f.router = &fakeRouter{migrated: true, policies: map[string]Policy{
		"e-acme":     {Endpoint: u.srv.URL, Key: "key-acme", IncludePrompts: true, IncludeToolContent: true},
		"e-clientco": {Endpoint: u.srv.URL, Key: "key-clientco"},
		"e-personal": {Endpoint: u.srv.URL, Key: "key-personal", IncludePrompts: true, IncludeToolContent: true},
	}}
	return f
}

func (f *teamFixture) start(t *testing.T) (*Relay, *httptest.Server) {
	r := newRelay(Options{Dir: f.dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Router: f.router,
		// The single-team rules would send everything to p-legacy: never used once migrated.
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: "http://127.0.0.1:1", Key: "key-legacy-rules", IncludePrompts: true, IncludeToolContent: true}, nil
		},
		CatchAll: func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-legacy"}, true },
	})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

func timedLogs(session string, at ...time.Time) *logspb.LogsData {
	var recs []*logspb.LogRecord
	for i, ts := range at {
		recs = append(recs, &logspb.LogRecord{TimeUnixNano: uint64(ts.UnixNano()), Attributes: []*commonpb.KeyValue{
			kv("session.id", session), kv("seq", string(rune('a'+i))), kv("prompt", "secret")}})
	}
	return &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}}}}}
}

// On a machine not yet migrated the router is never asked: the single-team rules decide,
// as they always have.
func TestAMachineNotMigratedRoutesAsBefore(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	router := &fakeRouter{migrated: false}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Router: router,
		Resolve: func(c claim.Claim) (Policy, error) {
			if p, ok := allPolicies(u)[c.ProjectID]; ok {
				return p, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, r)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	postProto(t, srv, "/v1/logs", mixedLogs())
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	got, projects := u.logs(t)
	if len(got["Bearer key-p1"]) != 1 || len(got["Bearer key-p2"]) != 2 || projects["Bearer key-p1"] != "p1" {
		t.Fatalf("forwarded by key = %v, projects %v", got, projects)
	}
	if n := len(router.records()); n != 0 {
		t.Fatalf("the router was asked %d times before the migration", n)
	}
	if _, err := os.Stat(filepath.Join(r.opts.Dir, teamsDir)); err == nil {
		t.Fatal("an enrollment queue was written before the migration")
	}
}

// Once migrated, each record goes where the router sends it, with its team's key alone,
// stamped with its team's project and filtered by its team's content rules, queued under
// its enrollment with its run and time.
func TestAMigratedRelaySendsEachRecordToItsTeam(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	r, srv := f.start(t)
	at := f.clock().Add(-time.Second)
	postProto(t, srv, "/v1/logs", timedLogs("A", at))
	postProto(t, srv, "/v1/logs", timedLogs("B", at))
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	got, projects := u.logs(t)
	if len(got["Bearer key-acme"]) != 1 || len(got["Bearer key-clientco"]) != 1 || len(got) != 2 {
		t.Fatalf("forwarded by key = %v", got)
	}
	if projects["Bearer key-acme"] != "acme" || projects["Bearer key-clientco"] != "clientco" {
		t.Fatalf("project stamps = %v", projects)
	}
	if attr(got["Bearer key-acme"][0].Attributes, "prompt") != "secret" || attr(got["Bearer key-clientco"][0].Attributes, "prompt") == "secret" {
		t.Fatal("a team's content rules were not its own")
	}
	recs := f.router.records()
	if len(recs) != 2 || !recs[0].Placed || recs[0].Session != "A" || !recs[0].At.Equal(at) {
		t.Fatalf("routed %+v", recs)
	}
	f.router.mu.Lock()
	rechecked := slices.Clone(f.router.rechecked)
	f.router.mu.Unlock()
	if len(rechecked) != 2 || rechecked[0].Session == "" || rechecked[0].At.IsZero() {
		t.Fatalf("rechecked %+v: each entry carries its run and time", rechecked)
	}
}

// A part whose records were made under two placements is split, and each piece goes to its
// own placement's team.
func TestAPartSpanningPlacementsIsSplit(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	base := f.clock().Add(-time.Hour)
	f.claims["S"] = claim.Claim{Kind: claim.KindRepository, Placements: []claim.Placement{
		{ProjectID: "acme", Kind: claim.KindRepository, Run: "r1", Repository: config.Repository{Origin: "github.com/acme/api"}, Since: base},
		{ProjectID: "clientco", Kind: claim.KindRepository, Run: "r2", Repository: config.Repository{Origin: "github.com/clientco/app"}, Since: base.Add(30 * time.Minute)},
	}}
	r, srv := f.start(t)
	postProto(t, srv, "/v1/logs", timedLogs("S", base.Add(time.Minute), base.Add(40*time.Minute), base.Add(2*time.Minute)))
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	got, projects := u.logs(t)
	if len(got["Bearer key-acme"]) != 2 || len(got["Bearer key-clientco"]) != 1 {
		t.Fatalf("forwarded by key = %v", got)
	}
	if projects["Bearer key-acme"] != "acme" || projects["Bearer key-clientco"] != "clientco" {
		t.Fatalf("project stamps = %v", projects)
	}
	// clientco withholds content, so only acme's records still say which they were.
	var seqs []string
	for _, lr := range got["Bearer key-acme"] {
		seqs = append(seqs, attr(lr.Attributes, "seq"))
	}
	if slices.Sort(seqs); !slices.Equal(seqs, []string{"a", "c"}) {
		t.Fatalf("acme got records %v, want the two made under its placement", seqs)
	}
}

// A Mixed run's record is dropped at once, counted against its run on this machine only,
// and the run's queued entries from the Mixed time on are dropped with it; one made before
// it stays.
func TestAMixedRunDropsItsRecordsAndItsQueue(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	u.status = 503 // nothing leaves: queued entries stay queued
	f := newTeamFixture(t, u)
	r, srv := f.start(t)
	now := f.clock()
	rt := teamRoute{enrollment: "e-acme", tool: "claude-code"}
	postProto(t, srv, "/v1/logs", timedLogs("A", now.Add(-2*time.Minute)))
	postProto(t, srv, "/v1/logs", timedLogs("A", now.Add(-time.Second)))
	waitFor(t, func() bool { return len(r.outbox.teamNames(rt)) == 2 })
	f.mu.Lock()
	f.claims["A"] = claim.Claim{Kind: claim.KindMixed, Placements: []claim.Placement{{Kind: claim.KindMixed}}}
	f.mu.Unlock()
	// The Mixed time is this record's: the fake router says so.
	postProto(t, srv, "/v1/logs", timedLogs("A", now.Add(-time.Minute)))
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.mixed_run.logs"] == 2 })
	snap := r.Stats().Snapshot()
	if snap.Blocked["mixed:A"] != 2 {
		t.Fatalf("blocked = %v", snap.Blocked)
	}
	for name := range snap.Counters {
		if strings.Contains(name, "mixed:") {
			t.Fatalf("a session's name reached the heartbeat counters: %v", snap.Counters)
		}
	}
	if left := r.outbox.teamNames(rt); len(left) != 1 {
		t.Fatalf("left queued: %v; want the part made before the run turned Mixed", left)
	}
}

// A record held for a pending binding or a blocked team is dropped when its hold runs out,
// counted by what blocked it.
func TestAHeldRecordIsCountedByItsBlocker(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	f.router.hold = &Routed{Outcome: Hold, Why: WhyPending, Blocker: "pending:github.com/acme"}
	r, srv := f.start(t)
	postProto(t, srv, "/v1/logs", logsOf("A", 2))
	waitForCounters(t, r, func(c map[string]int) bool { return c["held_parts"] == 1 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	snap := r.Stats().Snapshot()
	if snap.Counters["dropped.pending_binding.logs"] != 2 || snap.Blocked["pending:github.com/acme"] != 2 {
		t.Fatalf("counters %v, blocked %v", snap.Counters, snap.Blocked)
	}
	if got, _ := u.logs(t); len(got) != 0 {
		t.Fatal("a held record was sent")
	}
}

// A queued entry is rechecked before it is sent: one whose team must wait stays queued
// without holding up another team, and one whose team refuses it is dropped, never sent.
func TestQueuedEntriesAreRecheckedBeforeSending(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	f.router.waiting = map[string]bool{"e-acme": true}
	f.router.discard = map[string]bool{"e-clientco": true}
	f.claims["P"] = claim.Claim{ProjectID: "personal", Kind: claim.KindRepository, Repository: config.Repository{Origin: "github.com/dawson/side"}}
	f.router.policies["e-personal"] = Policy{Endpoint: u.srv.URL, Key: "key-personal", IncludePrompts: true, IncludeToolContent: true}
	r, srv := f.start(t)
	postProto(t, srv, "/v1/logs", logsOf("A", 1))
	postProto(t, srv, "/v1/logs", logsOf("B", 1))
	postProto(t, srv, "/v1/logs", logsOf("P", 1))
	waitForCounters(t, r, func(c map[string]int) bool {
		return c["forwarded.logs"] == 1 && c["dropped.team_blocked.logs"] == 1
	})
	got, _ := u.logs(t)
	if len(got) != 1 || len(got["Bearer key-personal"]) != 1 {
		t.Fatalf("forwarded %v", got)
	}
	if n := len(r.outbox.teamNames(teamRoute{enrollment: "e-acme", tool: "claude-code"})); n != 1 {
		t.Fatalf("acme's waiting entry: %d queued", n)
	}
	// A queue that only waits keeps no relay awake.
	waitFor(t, func() bool { _, quiet := r.Idle(); return quiet })
	// Its team recovers: it is sent.
	f.router.set(func(fr *fakeRouter) { fr.waiting = nil })
	r.mu.Lock()
	s := r.teamSenders[teamRoute{enrollment: "e-acme", tool: "claude-code"}]
	r.mu.Unlock()
	s.poke()
	waitFor(t, func() bool {
		got, _ := u.logs(t)
		return len(got["Bearer key-acme"]) == 1
	})
}

// Unticking a team drops its queue.
func TestUntickingDropsTheEnrollmentsQueue(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	f.router.waiting = map[string]bool{"e-acme": true}
	r, srv := f.start(t)
	postProto(t, srv, "/v1/logs", logsOf("A", 3))
	rt := teamRoute{enrollment: "e-acme", tool: "claude-code"}
	waitFor(t, func() bool { return len(r.outbox.teamNames(rt)) == 1 })
	f.router.set(func(fr *fakeRouter) { fr.deselected = map[string]bool{"e-acme": true} })
	r.sweep()
	if c := r.Stats().Snapshot().Counters; c["dropped.team_deselected.logs"] != 3 || len(r.outbox.teamNames(rt)) != 0 {
		t.Fatalf("counters %v, left %v", c, r.outbox.teamNames(rt))
	}
}

// A record no placement locates goes to personal only where the router's Unknown rule lets
// it, marked as placed without a claim; one that arrived while it could not never does.
func TestUnknownRecordsFollowTheUnknownRule(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	f.router.unknownToPersonal = true
	r, srv := f.start(t)
	postProto(t, srv, "/v1/logs", logsOf("U", 1))
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	u.checkStamps(t, map[string]map[string]string{semconv.TermaRelayAttributionKey: {"Bearer key-personal": semconv.TermaRelayAttributionCatchAll}})

	f.router.set(func(fr *fakeRouter) { fr.unknownToPersonal = false })
	postProto(t, srv, "/v1/logs", logsOf("V", 1))
	waitForCounters(t, r, func(c map[string]int) bool { return c["held_parts"] == 1 })
	// Personal may take unknown records again, but this one arrived while it could not.
	f.router.set(func(fr *fakeRouter) { fr.unknownToPersonal = true })
	r.sweep()
	recs := f.router.records()
	if last := recs[len(recs)-1]; !last.Narrow || last.Session != "V" {
		t.Fatalf("last routed %+v: want V marked narrow on arrival", last)
	}
	if r.Stats().Snapshot().Counters["forwarded.logs"] != 1 {
		t.Fatal("a record that arrived narrow reached personal")
	}
}

// A part queued before the migration is sent to its project's cutover enrollment, with
// that team's key and only as its recheck allows; one for a project with no cutover
// enrollment is dropped.
func TestPartsQueuedBeforeTheMigrationMapToTheCutoverEnrollment(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	f.router.legacy = map[string]Queued{"acme": {Enrollment: "e-acme", Project: "acme", Kind: "repository", Repository: "github.com/acme/api"}}
	o := outbox{dir: f.dir}
	for _, project := range []string{"acme", "old-team"} {
		c := claim.Claim{ProjectID: project, Repository: config.Repository{Origin: "github.com/acme/api"}}
		body := mustMarshal(t, logsOf("A", 1))
		if err := o.put(routeOf(c), c.Repository, newEntry(f.clock(), Logs, 1), body); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := f.start(t)
	waitForCounters(t, r, func(c map[string]int) bool {
		return c["forwarded.logs"] == 1 && c["dropped.legacy_unmapped.logs"] == 1
	})
	got, _ := u.logs(t)
	if len(got) != 1 || len(got["Bearer key-acme"]) != 1 {
		t.Fatalf("forwarded %v", got)
	}
}

// A relay that restarts sends what an enrollment's queue held, with its destination.
func TestAnEnrollmentsQueueSurvivesARestart(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	q := Queued{Enrollment: "e-acme", Project: "acme", Kind: "repository", Repository: "github.com/acme/api", Session: "A", At: f.clock()}
	rt := teamRoute{enrollment: "e-acme", tool: "claude-code"}
	if err := (outbox{dir: f.dir}).putTeam(rt, q, newEntry(f.clock(), Logs, 1), mustMarshal(t, logsOf("A", 1))); err != nil {
		t.Fatal(err)
	}
	r, _ := f.start(t)
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	f.router.mu.Lock()
	defer f.router.mu.Unlock()
	if len(f.router.rechecked) != 1 || f.router.rechecked[0].Session != "A" || !f.router.rechecked[0].At.Equal(q.At) {
		t.Fatalf("rechecked %+v", f.router.rechecked)
	}
}

func mustMarshal(t *testing.T, m *logspb.LogsData) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An unplaced record waits as it did before the migration, by the same reasons: for its
// session's claim, a conversation start as long as a trace.
func TestAnUnplacedRecordWaitsAsBefore(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newTeamFixture(t, u)
	r, srv := f.start(t)
	start := &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{Resource: &resourcepb.Resource{}, ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{Attributes: []*commonpb.KeyValue{kv("session.id", "U"), kv("event.name", "codex.conversation_starts")}},
	}}}}}}
	postProto(t, srv, "/v1/logs", logsOf("W", 1))
	postProto(t, srv, "/v1/logs", start)
	waitForCounters(t, r, func(c map[string]int) bool { return c["held_parts"] == 2 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["dropped.unclaimed_expired.logs"] != 1 || !r.Holding() {
		t.Fatalf("counters %v: want W dropped as unclaimed, the conversation start still held", c)
	}
}
