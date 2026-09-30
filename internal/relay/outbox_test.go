package relay

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// countFiles counts the regular files under dir, temporary ones aside.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".tmp-") {
			n++
		}
		return nil
	})
	return n
}

// What a relay accepted for a claimed session and could not deliver — the gateway
// down, then the relay stopped — is delivered by the next relay on the same outbox:
// nothing acknowledged is lost to a restart, and every record is accounted for.
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

	first := New(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: resolve, Grace: 50 * time.Millisecond})
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
	second := New(Options{Dir: dir, Token: token, Lookup: f.lookup, Resolve: resolve})
	go second.Run(t.Context())
	waitFor(t, func() bool { return second.Stats().Snapshot().Counters["forwarded.logs"] == 6 })
	if c := second.Stats().Snapshot().Counters; c["recovered_from_outbox"] != 6 {
		t.Fatalf("second relay: %v", c)
	}
	if got.Load() != 1 {
		t.Fatalf("the recovered parts took %d requests, want 1 merged", got.Load())
	}
	waitFor(t, func() bool { return countFiles(t, dir) == 0 })
}

// Only claimed parts reach the disk: a session no hook claimed, a project with no key
// and a record naming no session are held in memory and dropped there.
func TestRelayWritesNothingUnclaimed(t *testing.T) {
	u := newUpstream(t)
	u.status = http.StatusServiceUnavailable // so claimed parts stay on disk to be seen
	f := newFixture()
	r, srv := f.relay(t, u, allPolicies(u))
	body, _ := proto.Marshal(mixedLogs())
	post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
	waitFor(t, func() bool { return countFiles(t, r.opts.Dir) == 2 })
	_ = filepath.WalkDir(r.opts.Dir, func(p string, d fs.DirEntry, err error) error {
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
	rt := route{project: "p1", tool: "claude-code"}
	e := newEntry(time.Now(), Logs, 2)
	body, _ := proto.Marshal(logsOf("A", 2))
	if err := (outbox{dir}).put(rt, e, body); err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	r := New(Options{Dir: dir, Token: token, Lookup: f.lookup, Now: f.clock})
	go r.Run(t.Context())
	waitFor(t, func() bool { _, idle := r.Idle(); return idle })
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("a keyless part was removed: %d files", n)
	}
}

// The janitor drops what is past its age, then the oldest past the size bound, and
// counts each as dropped.
func TestOutboxJanitorBounds(t *testing.T) {
	dir := t.TempDir()
	o := outbox{dir}
	rt := route{project: "p1", tool: noTool}
	old := newEntry(time.Now(), Logs, 4)
	body := []byte("x")
	if err := o.put(rt, old, body); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-maxOutboxAge - time.Hour)
	_ = os.Chtimes(filepath.Join(o.routeDir(rt), old.name), past, past)
	fresh := newEntry(time.Now(), Logs, 1)
	if err := o.put(rt, fresh, body); err != nil {
		t.Fatal(err)
	}
	r := New(Options{Dir: dir, Token: token})
	removed := r.sweepOutbox(time.Now())
	if removed[rt] != 1 || r.Stats().Snapshot().Counters["dropped.outbox_expired.logs"] != 4 {
		t.Fatalf("removed %v: %v", removed, r.Stats().Snapshot().Counters)
	}
	if entries, _ := o.list(rt); len(entries) != 1 || entries[0].name != fresh.name {
		t.Fatalf("left %v", entries)
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
	if validRoute(route{project: "../x", tool: noTool}) || validRoute(route{project: "p", tool: "a/b"}) {
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
