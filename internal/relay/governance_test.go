package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// governanceRig is a relay whose senders are named by pid (sender) and whose signed app is
// whatever signed says, with an upstream for every policy.
type governanceRig struct {
	r      *Relay
	u      *upstream
	f      *fixture
	srv    *httptest.Server
	sender atomic.Int64
}

func newGovernanceRig(t *testing.T, signed func(pid int, opened time.Time) bool, catchAll func() (claim.Claim, bool)) *governanceRig {
	t.Helper()
	g := &governanceRig{u: newUpstream(t), f: newFixture()}
	policies := allPolicies(g.u)
	policies["p-default"] = Policy{Endpoint: g.u.srv.URL, Key: "key-default"}
	g.r = newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: g.f.lookup, Now: g.f.clock,
		PeerPID:          func(netip.AddrPort, netip.AddrPort) (int, bool) { pid := int(g.sender.Load()); return pid, pid != 0 },
		GovernanceSender: signed, CatchAll: catchAll,
		Resolve: func(c claim.Claim) (Policy, error) {
			if pol, ok := policies[c.ProjectID]; ok {
				return pol, nil
			}
			return Policy{}, ErrNoKey
		}})
	runRelay(t, g.r)
	g.srv = httptest.NewUnstartedServer(g.r.Handler())
	g.srv.Config.ConnContext = g.r.ConnContext
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

// send posts m from pid on a connection of its own.
func (g *governanceRig) send(t *testing.T, pid int64, m proto.Message) {
	t.Helper()
	g.sendOn(t, &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}, pid, m)
}

// sendOn posts m on client's connection, from pid if the connection is new.
func (g *governanceRig) sendOn(t *testing.T, client *http.Client, pid int64, m proto.Message) {
	t.Helper()
	g.sender.Store(pid)
	body, _ := proto.Marshal(m)
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+"/v1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// expire lets every held part's hold run out.
func (g *governanceRig) expire() {
	g.f.advance(2 * time.Minute)
	g.r.sweep()
}

var governanceSurface = kv(semconv.TermaCaptureSurfaceKey, semconv.TermaCaptureSurfaceGovernance)

const desktopPID = 777

func signedApp(pid int, _ time.Time) bool { return pid == desktopPID }

// Terma Desktop's governance records name a claimed session but come from the app, which the
// claim does not name: the relay takes them from the signed app alone, and only them.
func TestRelayTakesGovernanceFromTheDesktopAppAlone(t *testing.T) {
	t.Parallel()
	g := newGovernanceRig(t, signedApp, nil)
	g.f.claim("A", claim.Claim{ProjectID: "p1", PIDs: []int{100, 101}})
	g.send(t, desktopPID, logsOf("A", 2, governanceSurface)) // the app's governance: forwarded
	g.send(t, 555, logsOf("A", 1, governanceSurface))        // another process claiming to be governance: not
	g.send(t, desktopPID, logsOf("A", 1))                    // the app sending anything else: not
	mixed := logsOf("A", 1, governanceSurface)
	mixed.ResourceLogs[0].ScopeLogs[0].LogRecords = append(mixed.ResourceLogs[0].ScopeLogs[0].LogRecords, logsOf("A", 1).ResourceLogs[0].ScopeLogs[0].LogRecords...)
	g.send(t, desktopPID, mixed)                             // governance with something else in one part: not
	g.send(t, desktopPID, logsOf("Z", 1, governanceSurface)) // a session no hook claimed: not
	waitFor(t, func() bool { return g.r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	g.expire()
	c := g.r.Stats().Snapshot().Counters
	if c["forwarded.logs"] != 2 || c["dropped.uncovered_process.logs"] != 4 || c["dropped.unclaimed_expired.logs"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	if got := g.u.seqs(t); len(got) != 2 {
		t.Fatalf("forwarded records %v; want the app's two governance records", got)
	}
}

// The app is judged with the process that held the connection when its sender was found: a
// connection another process opened gains nothing when the app later has that pid, and the
// signature is asked once per connection, not per export.
func TestGovernanceSenderIsJudgedOncePerConnection(t *testing.T) {
	t.Parallel()
	var app atomic.Int64
	app.Store(desktopPID)
	var asked atomic.Int64
	g := newGovernanceRig(t, func(pid int, _ time.Time) bool { asked.Add(1); return int64(pid) == app.Load() }, nil)
	g.f.claim("A", claim.Claim{ProjectID: "p1", PIDs: []int{100}})
	other := &http.Client{Transport: &http.Transport{}}
	g.sendOn(t, other, 555, logsOf("A", 1, governanceSurface))
	app.Store(555) // pid 555 is the app's now; the connection is still the first process's
	g.sendOn(t, other, 555, logsOf("A", 1, governanceSurface))
	desktop := &http.Client{Transport: &http.Transport{}}
	for range 3 {
		g.sendOn(t, desktop, 555, logsOf("A", 1, governanceSurface))
	}
	g.r.sweep() // the first connection's parts wait, and the app's behind them only until a sweep
	waitFor(t, func() bool { return g.r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	g.expire()
	if c := g.r.Stats().Snapshot().Counters; c["forwarded.logs"] != 3 || c["dropped.uncovered_process.logs"] != 2 {
		t.Fatalf("stats = %v", c)
	}
	if n := asked.Load(); n != 2 {
		t.Fatalf("signature asked %d times; want once for each of the two connections", n)
	}
}

// Global mode's catch-all takes what no claim places, but never governance, whoever sends it
// and whatever is beside it: governance leaves only for a session a hook claimed.
func TestGovernanceNeedsAClaimInGlobalMode(t *testing.T) {
	t.Parallel()
	g := newGovernanceRig(t, signedApp, func() (claim.Claim, bool) { return claim.Claim{ProjectID: "p-default"}, true })
	g.f.claim("A", claim.Claim{ProjectID: "p1"})
	g.send(t, desktopPID, logsOf("Z", 1, governanceSurface)) // unclaimed: held, then dropped
	g.send(t, 555, logsOf("Z", 1, governanceSurface))        // the same from another process: the same
	mixed := logsOf("Z", 1, governanceSurface)
	mixed.ResourceLogs[0].ScopeLogs[0].LogRecords = append(mixed.ResourceLogs[0].ScopeLogs[0].LogRecords, logsOf("Z", 1).ResourceLogs[0].ScopeLogs[0].LogRecords...)
	g.send(t, desktopPID, mixed)                             // governance beside other records: the same
	g.send(t, 555, logsOf("Z", 2))                           // the agent's own, unclaimed: the catch-all's
	g.send(t, desktopPID, logsOf("A", 1, governanceSurface)) // claimed: its claim's
	waitFor(t, func() bool { return g.r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	g.expire()
	if c := g.r.Stats().Snapshot().Counters; c["forwarded.logs"] != 3 || c["dropped.unclaimed_expired.logs"] != 4 {
		t.Fatalf("stats = %v", c)
	}
	byAuth, _ := g.u.logs(t)
	if len(byAuth["Bearer key-default"]) != 2 || len(byAuth["Bearer key-p1"]) != 1 {
		t.Fatalf("delivered: default %d, p1 %d", len(byAuth["Bearer key-default"]), len(byAuth["Bearer key-p1"]))
	}
}

// governanceAt is one governance record of session at each time, seq its index.
func governanceAt(session string, at ...time.Time) *logspb.LogsData {
	m := logsOf(session, len(at), governanceSurface)
	for i, lr := range m.ResourceLogs[0].ScopeLogs[0].LogRecords {
		lr.TimeUnixNano = uint64(at[i].UnixNano())
	}
	return m
}

// A part whose records span the session's move to another placement is cut where it moved:
// each record goes where the claim put the session at its time, and a record from after a
// move to a repository the team does not collect never leaves under the one before.
func TestGovernancePartsAreCutWhereTheSessionMoved(t *testing.T) {
	t.Parallel()
	g := newGovernanceRig(t, signedApp, nil)
	t0 := g.f.now.Add(-3 * time.Hour)
	g.f.claim("A", claim.Claim{ProjectID: "p2", Placements: []claim.Placement{
		{ProjectID: "p1", PIDs: []int{100}, Since: t0},
		{ProjectID: "p2", PIDs: []int{200}, Since: t0.Add(time.Hour)},
		{ProjectID: "", PIDs: []int{300}, Since: t0.Add(2 * time.Hour)}, // not collected (claim.Mark)
	}})
	g.send(t, desktopPID, governanceAt("A", t0.Add(10*time.Minute), t0.Add(70*time.Minute), t0.Add(80*time.Minute), t0.Add(130*time.Minute)))
	waitFor(t, func() bool { return g.r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
	if c := g.r.Stats().Snapshot().Counters; c["dropped.not_collected.logs"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	byAuth, _ := g.u.logs(t)
	seqs := func(key string) (out []string) {
		for _, lr := range byAuth["Bearer "+key] {
			out = append(out, attr(lr.Attributes, "seq"))
		}
		return out
	}
	if p1, p2 := seqs("key-p1"), seqs("key-p2"); !slices.Equal(p1, []string{"0"}) || !slices.Equal(p2, []string{"1", "2"}) {
		t.Fatalf("p1 got %v, p2 got %v; want [0] and [1 2]", p1, p2)
	}
}

// A move the claim learns only after the part arrived: the part spans it, and is dropped
// rather than placed whole on either side.
func TestGovernancePartSpanningALaterMoveIsDropped(t *testing.T) {
	t.Parallel()
	g := newGovernanceRig(t, signedApp, nil)
	t0 := g.f.now.Add(-time.Hour)
	g.send(t, desktopPID, governanceAt("A2", t0, t0.Add(30*time.Minute))) // no claim yet: held
	g.f.claim("A2", claim.Claim{ProjectID: "p2", Placements: []claim.Placement{
		{ProjectID: "p1", Since: t0.Add(-time.Minute)},
		{ProjectID: "p2", Since: t0.Add(10 * time.Minute)},
	}})
	g.r.sweep()
	if c := g.r.Stats().Snapshot().Counters; c["dropped.spans_move.logs"] != 2 || c["forwarded.logs"] != 0 {
		t.Fatalf("stats = %v", c)
	}
}

// The pieces of a cut part are filtered and stamped apart: a piece under a policy that
// withholds content takes nothing from the next piece's scope or resource.
func TestGovernancePiecesKeepTheirOwnScope(t *testing.T) {
	t.Parallel()
	g := newGovernanceRig(t, signedApp, nil)
	t0 := g.f.now.Add(-2 * time.Hour)
	g.f.claim("A", claim.Claim{ProjectID: "p1", Placements: []claim.Placement{
		{ProjectID: "p2", Since: t0}, // withholds content (allPolicies)
		{ProjectID: "p1", Since: t0.Add(time.Hour)},
	}})
	m := governanceAt("A", t0.Add(10*time.Minute), t0.Add(70*time.Minute))
	m.ResourceLogs[0].ScopeLogs[0].Scope = &commonpb.InstrumentationScope{Name: "terma-cli", Attributes: []*commonpb.KeyValue{kv("x.note", "kept where content is")}}
	g.send(t, desktopPID, m)
	waitFor(t, func() bool { return g.r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	g.u.mu.Lock()
	defer g.u.mu.Unlock()
	notes, projects := map[string]string{}, map[string]string{}
	for _, req := range g.u.requests {
		var got logspb.LogsData
		if err := proto.Unmarshal(req.body, &got); err != nil {
			t.Fatal(err)
		}
		for _, rl := range got.ResourceLogs {
			projects[req.auth] = attr(rl.Resource.Attributes, semconv.MiradorProjectIDKey)
			notes[req.auth] = attr(rl.ScopeLogs[0].Scope.GetAttributes(), "x.note")
		}
	}
	if notes["Bearer key-p1"] == "" || notes["Bearer key-p2"] != "" {
		t.Fatalf("scope notes by key %v; want p1's kept and p2's withheld", notes)
	}
	if projects["Bearer key-p1"] != "p1" || projects["Bearer key-p2"] != "p2" {
		t.Fatalf("projects by key %v", projects)
	}
}

// With its placement history full, a session's oldest placements may have been dropped: a
// governance record from before the first kept has no known placement and does not leave.
func TestClaimDuringRefusesTimeBeforeAFullHistory(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_800_000_000, 0)
	var full []claim.Placement
	for i := range 8 {
		full = append(full, claim.Placement{ProjectID: fmt.Sprint("p", i), Since: t0.Add(time.Duration(i) * time.Hour)})
	}
	c := claim.Claim{Placements: full}
	if _, ok := c.During(t0.Add(-time.Minute)); ok {
		t.Error("During placed a time before a full history")
	}
	if got, ok := c.During(t0.Add(time.Minute)); !ok || got.ProjectID != "p0" {
		t.Errorf("During(t0+1m) = %q, %v; want p0", got.ProjectID, ok)
	}
	if got, ok := (claim.Claim{Placements: full[:2]}).During(t0.Add(-time.Minute)); !ok || got.ProjectID != "p0" {
		t.Errorf("During before a short history = %q, %v; want its first, p0", got.ProjectID, ok)
	}
}

// The app is the signed app's identifier under Mirador Labs' team, with a signature that holds,
// not Apple's, and a process that started before the connection it sent on opened.
func TestIsDesktop(t *testing.T) {
	t.Parallel()
	opened := time.Unix(1_800_000_000, 0)
	app := procinfo.Signature{Start: opened.Add(-time.Hour).UnixMicro(), Valid: true, Identifier: desktopIdentifier, TeamID: desktopTeamID}
	if !isDesktop(app, opened) {
		t.Fatal("the signed app, started before its connection: not the app")
	}
	for name, change := range map[string]func(*procinfo.Signature){
		"another team":            func(s *procinfo.Signature) { s.TeamID = "AAAAAAAAAA" },
		"another identifier":      func(s *procinfo.Signature) { s.Identifier = "ai.terma.other" },
		"an invalid signature":    func(s *procinfo.Signature) { s.Valid = false },
		"Apple's":                 func(s *procinfo.Signature) { s.Platform = true },
		"started after it opened": func(s *procinfo.Signature) { s.Start = opened.Add(time.Microsecond).UnixMicro() },
		"no start":                func(s *procinfo.Signature) { s.Start = 0 },
		"signed ad hoc, no team":  func(s *procinfo.Signature) { s.TeamID = "" },
	} {
		s := app
		change(&s)
		if isDesktop(s, opened) {
			t.Errorf("%s: taken for the app", name)
		}
	}
}

// With a team's content withheld, a governance record keeps its agent, host, kinds and signer
// and loses a tool's command line and a file's path.
func TestGovernanceRecordsKeepMetadataAndWithholdContent(t *testing.T) {
	t.Parallel()
	attrs := []*commonpb.KeyValue{
		kv("session.id", "A"),
		kv(semconv.TermaCaptureSurfaceKey, semconv.TermaCaptureSurfaceGovernance),
		kv("event.name", semconv.TermaNetworkConnectionEvent),
		kv(semconv.GenAIMainAgentNameKey, "claude-code"),
		kv("server.address", "proof.ovh.net"),
		kv(semconv.TermaNetworkProtocolKey, semconv.TermaNetworkProtocolHttps),
		kv(semconv.TermaProcessProgramKey, "curl"),
		kv(semconv.TermaProcessSignerKindKey, semconv.TermaProcessSignerKindApple),
		kv(semconv.TermaProcessCommandKey, "curl -sS -o 1mb.dat https://proof.ovh.net/files/1Mb.dat"),
		kv(semconv.TermaFilePathKey, "~/demo/1mb.dat"),
	}
	unclassified := map[string]int{}
	out, _ := testRules.withholdAttrs(attrs, false, false, false, nil, unclassified)
	got := map[string]bool{}
	for _, a := range out {
		got[a.GetKey()] = true
	}
	for _, k := range []string{semconv.GenAIMainAgentNameKey, "server.address", semconv.TermaNetworkProtocolKey, semconv.TermaProcessProgramKey,
		semconv.TermaProcessSignerKindKey, semconv.TermaCaptureSurfaceKey} {
		if !got[k] {
			t.Errorf("%s withheld; it is metadata", k)
		}
	}
	for _, k := range []string{semconv.TermaProcessCommandKey, semconv.TermaFilePathKey} {
		if got[k] {
			t.Errorf("%s left with tool content withheld", k)
		}
	}
	if len(unclassified) != 0 {
		t.Errorf("unclassified = %v", unclassified)
	}
}

func TestClaimDuringIsThePlacementInForceWhoeverSent(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_800_000_000, 0)
	c := claim.Claim{Placements: []claim.Placement{
		{ProjectID: "p1", PIDs: []int{100}, Since: t0},
		{ProjectID: "p2", PIDs: []int{200}, Since: t0.Add(time.Hour)},
	}}
	if got, ok := c.During(t0.Add(time.Minute)); !ok || got.ProjectID != "p1" {
		t.Errorf("During(t0+1m) = %q, %v; want p1", got.ProjectID, ok)
	}
	if got, ok := c.During(t0.Add(2 * time.Hour)); !ok || got.ProjectID != "p2" {
		t.Errorf("During(t0+2h) = %q, %v; want p2", got.ProjectID, ok)
	}
	if _, ok := c.At(300, t0.Add(time.Minute)); ok {
		t.Error("At placed a process no placement names")
	}
}
