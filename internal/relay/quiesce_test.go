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

// Work beside the engine (an image's upload) keeps the relay from quiescing until it halts,
// which it is asked last; and an export the relay accepts is told on, for the image sender
// to wake.
func TestWorkBesideTheEngineKeepsTheRelay(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
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
	// A relay with nothing else to do, asked to halt its work only once nothing else keeps it.
	var uploading atomic.Bool
	var asked atomic.Int64
	quiet := newRelay(Options{Dir: t.TempDir(), Token: token, Halt: func(bool) bool { asked.Add(1); return !uploading.Load() }})
	uploading.Store(true)
	if quiet.Quiesce(0) {
		t.Fatal("quiesced while an upload is under way")
	}
	if quiet.Quiesce(time.Hour) || asked.Load() != 1 {
		t.Fatalf("asked the work to halt %d times; want only when nothing else kept the relay", asked.Load())
	}
	uploading.Store(false)
	if !quiet.Quiesce(0) {
		t.Fatal("did not quiesce once the upload was over")
	}
}

// A relay exits for being idle only after the quiet asked for, and once the work beside it
// halts, not finally: a new export may yet keep it.
func TestTryIdleExit(t *testing.T) {
	t.Parallel()
	var ahead atomic.Int64
	var uploading atomic.Bool
	var finals atomic.Int64
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		Now: func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) },
		Halt: func(final bool) bool {
			if final {
				finals.Add(1)
			}
			return !uploading.Load()
		}})
	if r.TryIdleExit(time.Hour) || r.TryIdleExit(0) {
		t.Fatal("exited before the quiet asked for, or with none asked")
	}
	ahead.Store(int64(2 * time.Hour))
	uploading.Store(true)
	if r.TryIdleExit(time.Hour) {
		t.Fatal("exited mid-upload")
	}
	uploading.Store(false)
	if !r.TryIdleExit(time.Hour) || finals.Load() != 0 {
		t.Fatalf("did not exit once idle and halted, or asked a final halt (%d)", finals.Load())
	}
}

// A final halt asked by a quiesce the relay then does not stop for (an export came) is
// resumed; one it stops for is not.
func TestAQuiesceThatDoesNotStopResumes(t *testing.T) {
	t.Parallel()
	u := newUpstream(t)
	var ahead, resumed atomic.Int64
	var uploading atomic.Bool
	uploading.Store(true)
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		Now:    func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) },
		Halt:   func(bool) bool { return !uploading.Load() },
		Resume: func() { resumed.Add(1) },
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "key-p1", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	ahead.Store(int64(time.Hour))
	if r.Quiesce(time.Minute) {
		t.Fatal("quiesced mid-upload")
	}
	// The sender stops at the image's end; an export comes before the relay stops.
	ahead.Store(0)
	body, err := proto.Marshal(logsOf("session", 1))
	if err != nil {
		t.Fatal(err)
	}
	if code := post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false); code != http.StatusOK {
		t.Fatalf("export = %d", code)
	}
	if r.Quiesce(time.Minute) || resumed.Load() != 1 {
		t.Fatalf("quiesced with a fresh export, or resumed the sender %d times", resumed.Load())
	}
	if r.Quiesce(time.Minute); resumed.Load() != 1 {
		t.Fatal("resumed a halt it had not asked for again")
	}
}
