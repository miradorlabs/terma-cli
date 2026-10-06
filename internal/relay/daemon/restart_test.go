package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type relayRun struct {
	cancel context.CancelFunc
	up     chan struct{}
	// done is closed once Run returned res.
	done chan struct{}
	res  Result
}

func startRun(t *testing.T, c Config) *relayRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &relayRun{cancel: cancel, up: make(chan struct{}), done: make(chan struct{})}
	c.Listening = func(net.Addr, time.Duration) { close(r.up) }
	go func() {
		defer close(r.done)
		var err error
		if r.res, err = Run(ctx, c); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
		}
	})
	return r
}

func (r *relayRun) await(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.up:
	case <-r.done:
		t.Fatalf("%s stopped before it listened", what)
	case <-time.After(15 * time.Second):
		t.Fatalf("%s never listened", what)
	}
}

// A stopped relay closes its port and drops what it held, counting it: held records live
// in memory alone, so the next relay starts with none of them.
func TestARestartDropsWhatTheRelayHeld(t *testing.T) {
	stateDir, dir, token := setUpRelay(t)
	addr := freeAddr(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = addr
	old := startRun(t, c)
	old.await(t, "the service's relay")
	postSessionlessSpans(t, addr, token, 5)
	old.cancel()
	<-old.done
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("the stopped relay's address still accepts connections")
	}
	next := startRun(t, c)
	next.await(t, "the next relay")
	next.cancel()
	<-next.done

	before, after := counters(t, filepath.Join(dir, PrevStatsFile)), counters(t, filepath.Join(dir, StatsFile))
	if before["dropped.no_session_trace_at_exit.traces"] != 5 {
		t.Fatalf("the stopped relay dropped %d held spans at exit, want 5: %v", before["dropped.no_session_trace_at_exit.traces"], before)
	}
	if after["held_parts"] != 0 || after["dropped.no_session_trace_at_exit.traces"] != 0 {
		t.Fatalf("the next relay held what the stopped one had: %v", after)
	}
}

func postSessionlessSpans(t *testing.T, addr, tokenPath string, n int) {
	t.Helper()
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	var spans []*tracepb.Span
	for range n {
		spans = append(spans, &tracepb.Span{Name: "fs.read_file", TraceId: []byte("0123456789abcdef")})
	}
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(tok)))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %s", resp.Status)
	}
}

func counters(t *testing.T, path string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snap relay.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	return snap.Counters
}

// A relay stopped while it reads an export answers it and accounts for what it held: the
// final sweep waits for the handler, so the span is counted at exit, never lost unseen.
func TestARelayCountsTheExportItWasReadingWhenStopped(t *testing.T) {
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = freeAddr(t)
	r := startRun(t, c)
	r.await(t, "the relay")
	if !exportAcrossTheStop(t, c.Addr, token, r.cancel, 500*time.Millisecond) {
		t.Fatal("the export the relay was reading when stopped was not answered")
	}
	<-r.done
	if got := counters(t, filepath.Join(dir, StatsFile))["dropped.no_session_trace_at_exit.traces"]; got != 1 {
		t.Fatalf("the relay counted %d spans at exit, want 1", got)
	}
}

// An export still arriving when the shutdown stops waiting for it is cut off, not
// answered and then lost.
func TestAnExportTheShutdownGivesUpOnIsCutOff(t *testing.T) {
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = freeAddr(t)
	r := startRun(t, c)
	r.await(t, "the relay")
	if exportAcrossTheStop(t, c.Addr, token, r.cancel, 7*time.Second) {
		t.Fatal("an export the shutdown gave up on was answered")
	}
	<-r.done
	if got := counters(t, filepath.Join(dir, StatsFile))["dropped.no_session_trace_at_exit.traces"]; got != 0 {
		t.Fatalf("a cut-off export was held: %d spans counted at exit", got)
	}
}

// exportAcrossTheStop sends half of a one-span export, stops the relay, sends the rest
// after pause and reports whether the relay answered 200.
func exportAcrossTheStop(t *testing.T, addr, tokenPath string, stop func(), pause time.Duration) bool {
	t.Helper()
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{
		Spans: []*tracepb.Span{{Name: "fs.read_file", TraceId: []byte("0123456789abcdef")}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	half := len(body) / 2
	fmt.Fprintf(conn, "POST /v1/traces HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer %s\r\nContent-Type: application/x-protobuf\r\nContent-Length: %d\r\n\r\n%s",
		bytes.TrimSpace(tok), len(body), body[:half])
	time.Sleep(100 * time.Millisecond) // the handler is reading the body
	stop()
	time.Sleep(pause)
	_, _ = conn.Write(body[half:])
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// A newer terma's hook asks the relay of an earlier release to make way: the relay steps
// aside, and asks to be started again, as the newer terma.
func TestARelayStepsAsideForANewerRelease(t *testing.T) {
	runsThis(t, true)
	stateDir, _, _ := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Version = "1.2.0"
	r := startRun(t, c)
	awaitRecord(t, r, claim.Dir(stateDir))
	Spawn(stateDir, "1.3.0")
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running the earlier release")
	}
	if !r.res.Replaced || !r.res.Restart() {
		t.Fatalf("Run = %+v, want Replaced and a restart", r.res)
	}
}

// Only a later release takes over: the same or an earlier one, a build that is not a
// release, and a service that would start another install again all leave the relay be.
func TestARelayKeepsRunningUnlessALaterReleaseWouldReplaceIt(t *testing.T) {
	for _, tc := range []struct {
		name, relay, hook string
		runsThis          bool
	}{
		{"same release", "1.2.0", "1.2.0", true},
		{"earlier release", "1.3.0", "1.2.0", true},
		{"source build", "1.2.0", "v1.2.0-4-g401af35", true},
		{"relay is a development build", "dev", "1.3.0", true},
		{"service runs another install", "1.2.0", "1.3.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runsThis(t, tc.runsThis)
			stateDir, dir, _ := setUpRelay(t)
			c := runConfig(stateDir, 0, nil)
			c.Version, c.Service = tc.relay, true
			r := startRun(t, c)
			awaitRecord(t, r, dir)
			Spawn(stateDir, tc.hook)
			if _, err := os.Stat(filepath.Join(dir, ReplaceFile)); !os.IsNotExist(err) {
				t.Fatalf("asked the relay to make way: %v", err)
			}
			select {
			case <-r.done:
				t.Fatalf("the relay stepped aside: %+v", r.res)
			case <-time.After(2 * time.Second):
			}
		})
	}
}

// A relay asked to make way while it holds waits for its hold to empty, and steps aside once it has.
func TestAReplacedRelayWaitsForItsHoldToEmpty(t *testing.T) {
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = freeAddr(t)
	var ahead atomic.Int64
	c.Engine.Now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	r := startRun(t, c)
	r.await(t, "the relay")
	postSessionlessSpans(t, c.Addr, token, 5)
	askToMakeWay(t, dir)
	select {
	case <-r.done:
		t.Fatalf("the relay stepped aside while it held: %+v", r.res)
	case <-time.After(3 * time.Second):
	}
	ahead.Store(int64(relay.DefaultTraceHold + time.Minute)) // the held spans expire
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running once its hold was empty")
	}
	if !r.res.Replaced {
		t.Fatalf("Run = %+v, want Replaced", r.res)
	}
}

// A relay asked to make way whose hold never empties steps aside replacedMaxWait after the
// request, and counts what it still held as dropped at exit.
func TestAReplacedRelayStepsAsideAtTheCapWhileItHolds(t *testing.T) {
	was := replacedMaxWait
	replacedMaxWait = 3 * time.Second
	t.Cleanup(func() { replacedMaxWait = was })
	stateDir, dir, token := setUpRelay(t)
	c := runConfig(stateDir, 0, nil)
	c.Addr = freeAddr(t)
	r := startRun(t, c)
	r.await(t, "the relay")
	postSessionlessSpans(t, c.Addr, token, 5)
	askToMakeWay(t, dir)
	asked := time.Now()
	select {
	case <-r.done:
	case <-time.After(replacedMaxWait + 5*time.Second):
		t.Fatal("the relay kept running past the cap")
	}
	if waited := time.Since(asked); waited < replacedMaxWait {
		t.Fatalf("the relay stepped aside %v after the request, before the cap", waited)
	}
	if !r.res.Replaced {
		t.Fatalf("Run = %+v, want Replaced", r.res)
	}
	if got := counters(t, filepath.Join(dir, StatsFile))["dropped.no_session_trace_at_exit.traces"]; got != 5 {
		t.Fatalf("the relay dropped %d held spans at exit, want 5", got)
	}
}

// A request naming another relay's pid, one that has since exited, is cleared and ignored.
func TestARequestForAnotherRelayIsIgnored(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	path := filepath.Join(dir, ReplaceFile)
	if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startRun(t, runConfig(stateDir, 0, nil))
	r.await(t, "the relay")
	select {
	case <-r.done:
		t.Fatalf("the relay stepped aside for another's request: %+v", r.res)
	case <-time.After(2 * time.Second):
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the stale request was kept: %v", err)
	}
}

// awaitRecord waits until the relay r has recorded itself, which it does just after it listens.
func awaitRecord(t *testing.T, r *relayRun, dir string) {
	t.Helper()
	r.await(t, "the relay")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := RunningRelay(dir); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay never recorded itself")
		}
	}
}

// askToMakeWay is a newer terma's request to the relay running in this process.
func askToMakeWay(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ReplaceFile), fmt.Appendf(nil, "%d\n", os.Getpid()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runsThis stands in for whether the installed service starts this terma.
func runsThis(t *testing.T, yes bool) {
	was := serviceRunsThis
	serviceRunsThis = func(string) bool { return yes }
	t.Cleanup(func() { serviceRunsThis = was })
}
