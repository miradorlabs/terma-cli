package relay

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), pbSuffix) && !strings.HasPrefix(d.Name(), ".tmp-") {
			n++
		}
		return nil
	})
	return n
}

// What a stopped relay could not deliver is delivered by the next relay on the same outbox.
func TestRelayOutboxSurvivesARestart(t *testing.T) {
	var up atomic.Bool
	var got atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		got.Add(1)
	}))
	defer host.Close()
	dir := t.TempDir()
	f := newFixture()
	resolve := func(claim.Claim) (Policy, error) {
		return Policy{Endpoint: host.URL, Key: "k", IncludePrompts: true, IncludeToolContent: true}, nil
	}

	first := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: resolve, Grace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { first.Run(ctx); close(done) }()
	srv := httptest.NewServer(first.Handler())
	for range 3 {
		body, _ := proto.Marshal(logsOf("A", 2))
		if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
			t.Fatalf("export: %d", code)
		}
	}
	srv.Close()
	waitFor(t, func() bool { return first.Stats().Snapshot().Counters["upstream_retries"] >= 1 })
	cancel()
	<-done
	c := first.Stats().Snapshot().Counters
	if c["queued_at_exit.logs"] != 6 || sum(c, "dropped.") != 0 || c["forwarded.logs"] != 0 {
		t.Fatalf("first relay: %v", c)
	}
	if n := countFiles(t, dir); n != 3 {
		t.Fatalf("outbox holds %d parts after the first relay, want 3", n)
	}

	up.Store(true)
	second := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: resolve})
	runRelay(t, second)
	waitFor(t, func() bool { return second.Stats().Snapshot().Counters["forwarded.logs"] == 6 })
	if c := second.Stats().Snapshot().Counters; c["recovered_from_outbox"] != 6 {
		t.Fatalf("second relay: %v", c)
	}
	if got.Load() != 1 {
		t.Fatalf("the recovered parts took %d requests, want 1 merged", got.Load())
	}
	waitFor(t, func() bool { return countFiles(t, dir) == 0 })
}

// Only claimed, keyed parts reach the outbox; everything else waits in the hold, whose
// store is the hold's own (holdstore.go).
func TestRelayWritesNothingUnclaimed(t *testing.T) {
	u := newUpstream(t)
	u.status = http.StatusServiceUnavailable // so claimed parts stay on disk to be seen
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return countFiles(t, r.opts.Dir) == 2 })
	_ = filepath.WalkDir(r.opts.Dir, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() && d.Name() == heldDir {
			return filepath.SkipDir
		}
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		data, _ := os.ReadFile(p)
		for _, secret := range []string{"personal", "not opted in", "secret B"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s holds %q", p, secret)
			}
		}
		return nil
	})
	for _, want := range []string{"p1", "p2"} {
		if _, err := os.Stat(filepath.Join(r.opts.Dir, want)); err != nil {
			t.Errorf("no outbox for %s: %v", want, err)
		}
	}
}

// A route whose key is gone waits, and keeps no relay awake: only a new key can move it.
func TestRelayKeylessOutboxDoesNotKeepTheRelayBusy(t *testing.T) {
	dir := t.TempDir()
	rt := route{project: "p1", tool: "claude-code", repo: noRepo}
	e := newEntry(time.Now(), Logs, 2)
	body, _ := proto.Marshal(logsOf("A", 2))
	if err := (outbox{dir}).put(rt, config.Repository{}, e, body); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	r := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Now: f.clock})
	runRelay(t, r)
	waitFor(t, func() bool { _, idle := r.Idle(); return idle })
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("a keyless part was removed: %d files", n)
	}
}

// The janitor drops what is past its age, then the oldest past the size bound, counting each.
func TestOutboxJanitorBounds(t *testing.T) {
	dir := t.TempDir()
	o := outbox{dir}
	rt := route{project: "p1", tool: noTool, repo: noRepo}
	old := newEntry(time.Now(), Logs, 4)
	body := []byte("x")
	if err := o.put(rt, config.Repository{}, old, body); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-maxOutboxAge - time.Hour)
	_ = os.Chtimes(filepath.Join(o.routeDir(rt), old.name), past, past)
	fresh := newEntry(time.Now(), Logs, 1)
	if err := o.put(rt, config.Repository{}, fresh, body); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "p1", noTool, newEntry(time.Now(), Logs, 1).name)
	if err := os.WriteFile(stale, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRelay(Options{Dir: dir, Token: token})
	removed := r.sweepOutbox(time.Now())
	if removed[rt] != 1 || r.Stats().Snapshot().Counters["dropped.outbox_expired.logs"] != 4 {
		t.Fatalf("removed %v: %v", removed, r.Stats().Snapshot().Counters)
	}
	if entries, _ := o.list(rt); len(entries) != 1 || entries[0].name != fresh.name {
		t.Fatalf("left %v", entries)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a part outside any repository's route survived the sweep: %v", err)
	}
}

// A route's directory names its repository, and the outbox finds both again.
func TestOutboxRouteKeepsItsRepository(t *testing.T) {
	o := outbox{t.TempDir()}
	repo := config.Repository{Origin: "github.com/acme/web"}
	rt := routeOf(claim.Claim{ProjectID: "p1", Tool: "codex", Repository: repo})
	if rt.repo == noRepo || !validRoute(rt) {
		t.Fatalf("routeOf gave %v", rt)
	}
	if routeOf(claim.Claim{ProjectID: "p1"}).repo != noRepo {
		t.Fatal("no repository did not route to noRepo")
	}
	e := newEntry(time.Now(), Logs, 1)
	if err := o.put(rt, repo, e, []byte("x")); err != nil {
		t.Fatal(err)
	}
	routes, err := o.routes()
	if err != nil || len(routes) != 1 || routes[0] != rt {
		t.Fatalf("routes() = %v, %v; want [%v]", routes, err, rt)
	}
	if entries, _ := o.list(rt); len(entries) != 1 || entries[0] != e {
		t.Fatalf("list = %v", entries)
	}
	if got := o.identity(rt); got != repo {
		t.Fatalf("identity = %+v, want %+v", got, repo)
	}
}

func TestOutboxEntryNames(t *testing.T) {
	e := newEntry(time.Unix(0, 42), Traces, 7)
	got, ok := parseEntry(e.name)
	if !ok || got != e {
		t.Fatalf("parseEntry(%q) = %+v, %v", e.name, got, ok)
	}
	for _, bad := range []string{".tmp-123", "1-2-logs.pb", "x-000001-logs-1.pb", "1-000001-bogus-1.pb", "1-000001-logs-1.json"} {
		if _, ok := parseEntry(bad); ok {
			t.Errorf("parseEntry accepted %q", bad)
		}
	}
	if validRoute(route{project: "../x", tool: noTool, repo: noRepo}) || validRoute(route{project: "p", tool: "a/b", repo: noRepo}) ||
		validRoute(route{project: "p", tool: noTool, repo: ".."}) || validRoute(route{project: "p", tool: noTool}) {
		t.Fatal("validRoute admitted a path")
	}
}

// partialSuccess reads the rejected count out of any Export*ServiceResponse.
func TestPartialSuccess(t *testing.T) {
	inner := protowire.AppendTag(nil, 2, protowire.BytesType)
	inner = protowire.AppendString(inner, "bad records")
	inner = protowire.AppendTag(inner, 1, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 5)
	resp := protowire.AppendTag(nil, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, inner)
	if got := partialSuccess(resp); got != 5 {
		t.Fatalf("partialSuccess = %d, want 5", got)
	}
	for _, b := range [][]byte{nil, []byte("{}"), {0xff, 0xff}} {
		if got := partialSuccess(b); got != 0 {
			t.Fatalf("partialSuccess(%q) = %d", b, got)
		}
	}
}

func TestBackoffAndJitter(t *testing.T) {
	if got := nextBackoff(0, 0); got != minBackoff {
		t.Fatalf("first backoff %v", got)
	}
	if got := nextBackoff(maxBackoff, 0); got != maxBackoff {
		t.Fatalf("capped backoff %v", got)
	}
	if got := nextBackoff(time.Hour, refusedBackoff); got != maxRefusedBackoff {
		t.Fatalf("refused backoff %v", got)
	}
	for range 1000 {
		d := jitter(10*time.Second, 0)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("jitter out of ±20%%: %v", d)
		}
		if d := jitter(time.Second, 3*time.Second); d < 3*time.Second {
			t.Fatalf("jitter under the floor: %v", d)
		}
	}
	if retryAfter("120") != 2*time.Minute || retryAfter("99999") != maxRetryAfter || retryAfter("soon") != 0 {
		t.Fatal("retryAfter")
	}
}

// A part queued while prompts were allowed leaves under the policy at delivery, without them.
func TestRelayQueuedPartsFollowTheCurrentContentPolicy(t *testing.T) {
	var up atomic.Bool
	var got [][]byte
	var mu sync.Mutex
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
	}))
	defer host.Close()
	dir := t.TempDir()
	f := newFixture()
	policy := func(prompts bool) func(claim.Claim) (Policy, error) {
		return func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: host.URL, Key: "k", IncludePrompts: prompts, IncludeToolContent: true}, nil
		}
	}
	first := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: policy(true), Grace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { first.Run(ctx); close(done) }()
	srv := httptest.NewServer(first.Handler())
	body, _ := proto.Marshal(logsOf("A", 1, kv("event.name", "user_prompt"), kv("prompt", "TERMA_QUEUED_SECRET")))
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	srv.Close()
	waitFor(t, func() bool { return first.Stats().Snapshot().Counters["upstream_retries"] >= 1 })
	cancel()
	<-done

	up.Store(true)
	second := newRelay(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: policy(false)})
	runRelay(t, second)
	waitFor(t, func() bool { return second.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	mu.Lock()
	defer mu.Unlock()
	for _, b := range got {
		if strings.Contains(string(b), "TERMA_QUEUED_SECRET") {
			t.Fatal("a prompt queued before prompts were turned off reached upstream")
		}
	}
	if c := second.Stats().Snapshot().Counters; c["withheld_at_send_records"] == 0 {
		t.Fatalf("nothing was withheld at delivery: %v", c)
	}
}
