package relay

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Terma Desktop's governance records name a claimed session but come from the app, which the
// claim does not name: the relay takes them from the signed app alone, and only them.
func TestRelayTakesGovernanceFromTheDesktopAppAlone(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	f := newFixture()
	f.claims["A"] = claim.Claim{ProjectID: "p1", PIDs: []int{100, 101}}
	const app = 777
	var sender atomic.Int64
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock,
		PeerPID:          func(netip.AddrPort, netip.AddrPort) (int, bool) { pid := int(sender.Load()); return pid, pid != 0 },
		GovernanceSender: func(pid int) bool { return pid == app },
		Resolve:          func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }})
	runRelay(t, r)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.ConnContext = r.ConnContext
	srv.Start()
	defer srv.Close()
	send := func(pid int64, m proto.Message) {
		sender.Store(pid)
		body, _ := proto.Marshal(m)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	governance := kv(semconv.TermaCaptureSurfaceKey, semconv.TermaCaptureSurfaceGovernance)
	send(app, logsOf("A", 2, governance)) // the app's governance: forwarded
	send(555, logsOf("A", 1, governance)) // another process claiming to be governance: not
	send(app, logsOf("A", 1))             // the app sending anything else: not
	mixed := logsOf("A", 1, governance)
	mixed.ResourceLogs[0].ScopeLogs[0].LogRecords = append(mixed.ResourceLogs[0].ScopeLogs[0].LogRecords, logsOf("A", 1).ResourceLogs[0].ScopeLogs[0].LogRecords...)
	send(app, mixed)                      // governance with something else in one part: not
	send(app, logsOf("Z", 1, governance)) // a session no hook claimed: not
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 2 })
	f.mu.Lock()
	f.now = f.now.Add(2 * time.Minute)
	f.mu.Unlock()
	r.sweep()
	c := r.Stats().Snapshot().Counters
	if c["forwarded.logs"] != 2 || c["dropped.uncovered_process.logs"] != 4 || c["dropped.unclaimed_expired.logs"] != 1 {
		t.Fatalf("stats = %v", c)
	}
	if got := u.seqs(t); len(got) != 2 {
		t.Fatalf("forwarded records %v; want the app's two governance records", got)
	}
}

// With a team's content withheld, a governance record keeps its host, kinds and signer and
// loses a tool's command line and a file's path.
func TestGovernanceRecordsKeepMetadataAndWithholdContent(t *testing.T) {
	t.Parallel()
	attrs := []*commonpb.KeyValue{
		kv("session.id", "A"),
		kv(semconv.TermaCaptureSurfaceKey, semconv.TermaCaptureSurfaceGovernance),
		kv("event.name", semconv.TermaNetworkConnectionEvent),
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
	for _, k := range []string{"server.address", semconv.TermaNetworkProtocolKey, semconv.TermaProcessProgramKey,
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
