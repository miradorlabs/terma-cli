package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// setup's check-in reports the beat as sent, no team key here yet, another failure,
// or no relay at all.
func TestRelayCheckIn(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	_ = free.Close()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if ok, what := CheckIn(t.Context(), stateDir); ok || !strings.Contains(what, "did not answer") || time.Since(start) > 3*time.Second {
		t.Fatalf("no relay: %v %q after %v", ok, what, time.Since(start))
	}
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
		says   string
	}{
		{200, `{}`, true, "reported to your organization"},
		{502, `{"error":"no key for this team on this machine"}`, true, "once a repository is connected"},
		{502, `{"error":"invalid token"}`, false, "invalid token"},
	} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/heartbeat" || r.URL.Query().Get("reason") != "setup" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})}
		go func() { _ = srv.Serve(ln) }()
		unlock := pretendRunning(t, dir, addr)
		ok, what := CheckIn(t.Context(), stateDir)
		unlock()
		_ = srv.Close()
		if ok != tc.ok || !strings.Contains(what, tc.says) {
			t.Errorf("%d %s: %v %q", tc.status, tc.body, ok, what)
		}
	}
}

// pretendRunning holds dir's lock and records a relay listening on addr, as Run does.
func pretendRunning(t *testing.T, dir, addr string) func() {
	t.Helper()
	unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, RunFile), fmt.Appendf(nil, `{"pid":%d,"addr":%q}`, os.Getpid(), addr), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Remove(filepath.Join(dir, RunFile)); unlock() }
}

// Something else listening on the relay's address never gets the relay's token from setup's
// check-in: no relay holds the lock, or the one that does listens elsewhere.
func TestACheckInSendsTheTokenOnlyToTheRelay(t *testing.T) {
	stateDir, dir, _ := setUpRelay(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := os.WriteFile(filepath.Join(dir, AddrFile), []byte(addr), 0o600); err != nil {
		t.Fatal(err)
	}
	var asked atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		fmt.Fprint(w, `{}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if ok, what := CheckIn(t.Context(), stateDir); ok || !strings.Contains(what, "another process listens on "+addr) {
		t.Errorf("no relay running: %v %q", ok, what)
	}
	// A relay that runs, but on another address than the one recorded, is not what answers.
	unlock := pretendRunning(t, dir, freeAddr(t))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	ok, what := CheckIn(ctx, stateDir)
	cancel()
	unlock()
	if ok || !strings.Contains(what, "another process listens on "+addr) {
		t.Errorf("a relay on another address: %v %q", ok, what)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the check-in sent %d requests to a process that is not the relay", n)
	}
}

// A stopped relay's last heartbeat names its exit reason and what started it, so even an
// idle exit is counted upstream: the counters after the last interval beat, and the launch
// on the record's resource, say what kind of relay went. Run leaves it to its caller
// (ExitBeat), which sends it once the lock is free and the next relay under way.
func TestARunSendsItsExitHeartbeat(t *testing.T) {
	t.Parallel()
	stateDir, dir, _ := setUpRelay(t)
	var mu sync.Mutex
	var beats []*logspb.LogsData
	lockFree := false
	c := runConfig(stateDir, time.Millisecond, nil)
	c.Launch, c.Version = LaunchHook, "1.2.0"
	c.Engine.HeartbeatEvery = time.Hour
	c.Engine.HeartbeatSend = func(_ context.Context, b *logspb.LogsData) error {
		unlock, err := flock.TryLock(filepath.Join(dir, LockFile))
		if err == nil {
			unlock()
		}
		mu.Lock()
		beats = append(beats, b)
		lockFree = err == nil
		mu.Unlock()
		return nil
	}
	res, err := Run(t.Context(), c)
	if err != nil || res.Restart() || res.SetupGone || res.AlreadyRunning {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	mu.Lock()
	sent := len(beats)
	mu.Unlock()
	if sent != 0 {
		t.Fatalf("Run sent %d beats itself, want its caller to send the exit beat", sent)
	}
	res.ExitBeat()
	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 1 {
		t.Fatalf("%d beats, want the one exit beat", len(beats))
	}
	beat := beats[0]
	rec := beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if reason := attrOf(rec.Attributes, semconv.TermaRelayHeartbeatReasonKey); reason != semconv.TermaRelayHeartbeatReasonExit {
		t.Fatalf("exit beat reason = %q", reason)
	}
	if exit := attrOf(rec.Attributes, semconv.TermaRelayExitReasonKey); exit != semconv.TermaRelayExitReasonIdle {
		t.Fatalf("exit reason = %q", exit)
	}
	if launch := attrOf(beat.ResourceLogs[0].Resource.Attributes, semconv.TermaRelayLaunchKey); launch != semconv.TermaRelayLaunchHook {
		t.Fatalf("launch = %q", launch)
	}
	if !lockFree {
		t.Fatal("the exit beat was sent while the relay held its lock")
	}
}

func attrOf(kvs []*commonpb.KeyValue, key string) string {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value.GetStringValue()
		}
	}
	return ""
}

// A relay whose setup is gone sends no last heartbeat: teardown took away what it would
// report with, and sending one could mint a key the developer just removed.
func TestARelayWhoseSetupWentSendsNoExitHeartbeat(t *testing.T) {
	t.Parallel()
	stateDir, _, token := setUpRelay(t)
	var beats atomic.Int32
	c := runConfig(stateDir, 0, nil)
	c.Engine.HeartbeatEvery = time.Hour
	c.Engine.HeartbeatSend = func(context.Context, *logspb.LogsData) error { beats.Add(1); return nil }
	r := startRun(t, c)
	r.await(t, "the relay")
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay kept running once its setup went")
	}
	if !r.res.SetupGone {
		t.Fatalf("Run = %+v, want SetupGone", r.res)
	}
	r.res.ExitBeat()
	if n := beats.Load(); n != 0 {
		t.Fatalf("%d beats after teardown, want none", n)
	}
}

// The caller sends the last heartbeat of a relay a hook or a developer started, whatever
// stopped it, and never the service's: its manager starts the next relay only once the
// process exits, and nothing listens until then.
func TestLastBeatDue(t *testing.T) {
	for _, tc := range []struct {
		res  Result
		want bool
	}{
		{Result{}, true}, {Result{Replaced: true}, true}, {Result{Updated: true}, true},
		{Result{Service: true}, false}, {Result{Service: true, Replaced: true}, false}, {Result{Service: true, Updated: true}, false},
	} {
		if got := tc.res.LastBeatDue(); got != tc.want {
			t.Errorf("%+v.LastBeatDue() = %v, want %v", tc.res, got, tc.want)
		}
	}
}
