package relay

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// filesUnder is every file under dir, with its contents.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(p)
			files[p] = string(data)
		}
		return nil
	})
	return files
}

// What a relay holds lives in memory alone: nothing unclaimed reaches the disk while it
// waits, a claim still sends it on through the outbox, and a stopping relay drops and
// counts the rest, so the next relay on the same directory has none of it.
func TestRelayHoldsInMemoryOnly(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	u.status = http.StatusServiceUnavailable // so the claimed part stays in the outbox to be seen
	f := newFixture()
	dir := t.TempDir()
	opts := Options{Dir: dir, Token: token, Hold: time.Minute, Lookup: f.lookup, Now: f.clock, Grace: 50 * time.Millisecond,
		Resolve: func(c claim.Claim) (Policy, error) { return allPolicies(u)[c.ProjectID], nil }}
	first := newRelay(opts)
	srv, stop := startRelay(t, first)
	postProto(t, srv, "/v1/logs", logsOf("X", 2, kv("prompt", "work X")))
	postProto(t, srv, "/v1/logs", logsOf("Y", 2, kv("prompt", "personal Y")))
	postProto(t, srv, "/v1/traces", sessionlessSpans([]byte("0123456789abcdef"), 3))
	first.sweep()
	if c := first.Stats().Snapshot().Counters; c["held_parts"] != 3 {
		t.Fatalf("stats = %v", c)
	}
	if files := filesUnder(t, dir); len(files) != 0 {
		t.Fatalf("held records reached the disk: %v", files)
	}

	f.claim("X", claim.Claim{ProjectID: "p1"})
	first.sweep()
	waitFor(t, func() bool { return countFiles(t, dir) == 1 })
	stop()
	c := first.Stats().Snapshot().Counters
	if c["released_after_hold"] != 2 || c["dropped.unclaimed_expired_at_exit.logs"] != 2 || c["dropped.no_session_trace_at_exit.traces"] != 3 {
		t.Fatalf("stats = %v", c)
	}
	for p, data := range filesUnder(t, dir) {
		if strings.Contains(p, ".held") || strings.Contains(data, "personal Y") || strings.Contains(data, "fs.read_file") {
			t.Errorf("%s holds what was never claimed", p)
		}
	}
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("%d parts on disk after the stop; want the claimed one alone", n)
	}

	f.claim("Y", claim.Claim{ProjectID: "p1"})
	next := newRelay(opts)
	t.Cleanup(next.cancelSend)
	next.sweep()
	if c := next.Stats().Snapshot().Counters; c["held_parts"] != 0 || next.heldN != 0 {
		t.Fatalf("the next relay took back held records: %v", c)
	}
}

// postBench posts body to h's /v1/logs for each benchmark iteration.
func postBench(b *testing.B, h http.Handler, body []byte) {
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkRelayHeldExport is one export of an unclaimed session, which the relay holds.
func BenchmarkRelayHeldExport(b *testing.B) {
	f := newFixture()
	r := newRelay(Options{Dir: b.TempDir(), Token: token, Lookup: f.lookup})
	runRelay(b, r)
	body, _ := proto.Marshal(logsOf("P", 3))
	postBench(b, r.Handler(), body)
}

// BenchmarkRelayNotCollectedExport is BenchmarkRelayHeldExport's export from a session a
// hook marked not collected, which the relay drops on arrival.
func BenchmarkRelayNotCollectedExport(b *testing.B) {
	f := newFixture()
	f.claim("P", marked(f.now))
	r := newRelay(Options{Dir: b.TempDir(), Token: token, Lookup: f.lookup})
	runRelay(b, r)
	body, _ := proto.Marshal(logsOf("P", 3))
	postBench(b, r.Handler(), body)
	if c := r.Stats().Snapshot().Counters; c["held_parts"] != 0 {
		b.Fatalf("a marked session's export was held: %v", c)
	}
}
