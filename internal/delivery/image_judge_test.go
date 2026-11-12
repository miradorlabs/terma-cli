package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

// timeoutError is a net.Error that timed out, as a TLS handshake's does.
type timeoutError struct{}

func (timeoutError) Error() string   { return "net/http: TLS handshake timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// How a send went: delivered, deferred having learned nothing, stalled on the uplink (which
// backs off only the project's images and fails nothing), or failed on the host.
func TestJudgeImage(t *testing.T) {
	t.Parallel()
	live := t.Context()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()
	reset := &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Err: os.NewSyscallError("write", connectionBroken[0])}}
	deadline := &url.Error{Op: "Post", Err: context.DeadlineExceeded}
	for _, c := range []struct {
		name string
		pass context.Context
		err  error
		got  progress
		want imageOutcome
	}{
		{"delivered", live, nil, written, imageDelivered},
		{"no time left to start it", live, errNoTimeLeft, notReached, imageDeferred},
		{"pass cancelled mid-upload", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, reached, imageDeferred},
		{"pass cancelled before the host", cancelled, &url.Error{Op: "Post", Err: context.Canceled}, notReached, imageDeferred},
		{"pass ran out mid-upload", expired, deadline, reached, imageDeferred},
		{"pass ran out after the host answered", expired, &IngestError{Status: http.StatusInternalServerError}, written, imageFailed},
		{"pass ran out before the host", expired, deadline, notReached, imageDeferred},
		{"own deadline mid-upload", live, deadline, reached, imageStalled},
		{"reset mid-upload", live, reset, reached, imageStalled},
		{"cut off", live, &url.Error{Op: "Post", Err: io.EOF}, reached, imageStalled},
		{"cut off mid-answer", live, &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}, reached, imageStalled},
		{"closed under the write", live, &url.Error{Op: "Post", Err: &net.OpError{Op: "write", Err: net.ErrClosed}}, reached, imageStalled},
		{"no answer to a body all written", live, deadline, written, imageFailed},
		{"cut off after a body all written", live, &url.Error{Op: "Post", Err: io.EOF}, written, imageFailed},
		{"pass ran out after a body all written", expired, deadline, written, imageDeferred},
		{"host refused", live, &IngestError{Status: http.StatusInternalServerError}, written, imageFailed},
		{"gateway timeout", live, &IngestError{Status: http.StatusGatewayTimeout}, written, imageFailed},
		{"unprocessable", live, &IngestError{Status: http.StatusUnprocessableEntity}, written, imageFailed},
		{"too many requests", live, &IngestError{Status: http.StatusTooManyRequests}, written, imageUnavailable},
		{"bad gateway", live, &IngestError{Status: http.StatusBadGateway}, written, imageUnavailable},
		{"between deploys", live, &IngestError{Status: http.StatusServiceUnavailable}, written, imageUnavailable},
		{"key refused", live, &IngestError{Status: http.StatusUnauthorized}, written, imageUnavailable},
		{"dial", live, &url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, notReached, imageUnavailable},
		{"dns", live, &url.Error{Op: "Post", Err: &net.DNSError{Err: "no such host", Name: "x"}}, notReached, imageUnavailable},
		{"tls certificate", live, &url.Error{Op: "Post", Err: &tls.CertificateVerificationError{}}, notReached, imageUnavailable},
		{"tls handshake timeout", live, &url.Error{Op: "Post", Err: timeoutError{}}, notReached, imageUnavailable},
		{"own deadline before the host", live, deadline, notReached, imageUnavailable},
		{"something else", live, errors.New("odd"), reached, imageFailed},
	} {
		if got := judgeImage(c.pass, c.err, c.got); got != c.want {
			t.Errorf("%s: judgeImage = %v, want %v", c.name, got, c.want)
		}
	}
}

// A request that reached the host says so, and one whose body went out says that: an
// upload cut off says only it reached, the host's answer that its body went.
func TestPostSaysHowFarItGot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		cut  bool
		want progress
	}{{"answered", false, written}, {"cut off", true, reached}} {
		rec := &recorder{cut: func(string) bool { return c.cut }}
		srv := httptest.NewServer(rec)
		// The body outgrows every buffer between the two, so a cut leaves it half written.
		events := []spool.Event{{Name: "x", Attrs: map[string]any{"pad": strings.Repeat("x", 16<<20)}}}
		got, _ := (&otlpSender{Endpoint: srv.URL, APIKey: testKey}).post(t.Context(), events)
		srv.Close()
		if got != c.want {
			t.Errorf("%s: progress = %v, want %v", c.name, got, c.want)
		}
	}
	unreachable := &otlpSender{Endpoint: "http://127.0.0.1:1", APIKey: testKey}
	if got, err := unreachable.post(t.Context(), []spool.Event{{Name: "x"}}); err == nil || got != notReached {
		t.Errorf("no host: progress = %v, %v", got, err)
	}
}

// A request retried on a new connection starts over: a reused connection that failed before
// the request was written says nothing of the new one, and a failed write is not a body sent.
func TestProgressStartsOverOnANewConnection(t *testing.T) {
	t.Parallel()
	var got atomic.Int32
	trace := progressTrace(&got)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: io.EOF})
	if progress(got.Load()) != reached {
		t.Fatalf("a failed write = %v", progress(got.Load()))
	}
	trace.ConnectStart("tcp", "127.0.0.1:1")
	if progress(got.Load()) != notReached {
		t.Fatalf("a new connection's dial = %v", progress(got.Load()))
	}
	trace.GotConn(httptrace.GotConnInfo{})
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	if progress(got.Load()) != written {
		t.Fatalf("a body written = %v", progress(got.Load()))
	}
}

// Retry-After reads seconds or a date.
func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Time{
		"120":                           now.Add(2 * time.Minute),
		"99999999999999":                now.Add(time.Hour),
		"Thu, 12 Nov 2026 12:10:00 GMT": now.Add(10 * time.Minute),
		"Thu, 12 Nov 2026 11:00:00 GMT": {},
		"":                              {},
		"soon":                          {},
	} {
		if got := retryAfter(h, now); !got.Equal(want) {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
}
