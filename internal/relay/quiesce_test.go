package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// A relay quiesces only once no export has arrived for the pause asked for and none is being
// read; from then on it answers each export 503, which its exporter sends again, to the
// next relay, rather than 200 for a record it would then lose.
func TestAQuiescedRelayAsksForExportsAgain(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	var ahead atomic.Int64
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		Now:    func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) },
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, err := proto.Marshal(logsOf("session", 1))
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	if r.Quiesce(time.Minute) {
		t.Fatal("quiesced a minute's pause after an export just now")
	}

	// An export being read keeps it, however long the pause.
	ahead.Store(int64(2 * time.Minute))
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	fmt.Fprintf(conn, "POST /v1/logs HTTP/1.1\r\nHost: relay\r\nAuthorization: Bearer %s\r\nContent-Type: application/x-protobuf\r\nContent-Length: %d\r\n\r\n%s",
		token, len(body), body[:len(body)/2])
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.inflight == 1 })
	if r.Quiesce(0) {
		t.Fatal("quiesced while an export was being read")
	}
	_, _ = conn.Write(body[len(body)/2:])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the export being read = %d", resp.StatusCode)
	}

	ahead.Store(int64(4 * time.Minute))
	if !r.Quiesce(time.Minute) {
		t.Fatal("did not quiesce after a pause with nothing held or being read")
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("an export to a quiesced relay = %d (Retry-After %q), want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if got := r.Stats().Snapshot().Counters["refused_restarting"]; got != 1 {
		t.Fatalf("refused_restarting = %d, want 1", got)
	}
}

// Work beside the engine (an image's upload) keeps the relay from idling out or quiescing,
// and an export it accepts is told on, for the image sender to wake.
func TestWorkBesideTheEngineKeepsTheRelay(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	var busy atomic.Bool
	var accepted atomic.Int64
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		Accepted: func() { accepted.Add(1) },
		Lookup:   func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body, err := proto.Marshal(logsOf("session", 1))
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK || accepted.Load() != 1 {
		t.Fatalf("export = %d, told of %d", code, accepted.Load())
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", "wrong", false); code == http.StatusOK || accepted.Load() != 1 {
		t.Fatalf("a refused export = %d, told of %d", code, accepted.Load())
	}
	// A relay with nothing else to do.
	quiet := newRelay(Options{Dir: t.TempDir(), Token: token, Busy: busy.Load})
	busy.Store(true)
	if _, idle := quiet.Idle(); idle {
		t.Fatal("idle while an upload is under way")
	}
	if quiet.Quiesce(0) {
		t.Fatal("quiesced while an upload is under way")
	}
	busy.Store(false)
	if _, idle := quiet.Idle(); !idle {
		t.Fatal("not idle once the upload is over")
	}
	if !quiet.Quiesce(0) {
		t.Fatal("did not quiesce once the upload is over")
	}
}
