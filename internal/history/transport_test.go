package history

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/miradorlabs/terma-cli/internal/sendas"
)

type fakeSignIn struct {
	mu      sync.Mutex
	account string
	tokens  []string
	idx     int
	refused []string
	retired []string
}

func (f *fakeSignIn) Account() string { return f.account }
func (f *fakeSignIn) Token(_ context.Context, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if account != f.account || len(f.tokens) == 0 {
		return "", nil
	}
	tok := f.tokens[min(f.idx, len(f.tokens)-1)]
	return tok, nil
}
func (f *fakeSignIn) Refused(_ context.Context, tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, tok)
	if f.idx+1 < len(f.tokens) {
		f.idx++
	}
}
func (f *fakeSignIn) Retired(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retired = append(f.retired, tok)
}

func encodePartialSuccess(rejected int64, msg string) []byte {
	var ps []byte
	if rejected > 0 {
		ps = protowire.AppendTag(ps, 1, protowire.VarintType)
		ps = protowire.AppendVarint(ps, uint64(rejected))
	}
	if msg != "" {
		ps = protowire.AppendTag(ps, 2, protowire.BytesType)
		ps = protowire.AppendString(ps, msg)
	}
	var resp []byte
	resp = protowire.AppendTag(resp, 1, protowire.BytesType)
	resp = protowire.AppendBytes(resp, ps)
	return resp
}

func TestChunkLogsSplitsAtCapAndRejectsOversizedSingleRecord(t *testing.T) {
	t.Parallel()
	makeRec := func(size int) *logspb.LogRecord {
		return &logspb.LogRecord{
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.Repeat("x", size)}},
		}
	}
	batch := &logspb.LogsData{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "test"}}},
			}},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: "scope"},
				LogRecords: []*logspb.LogRecord{makeRec(600), makeRec(600), makeRec(600), makeRec(600)},
			}},
		}},
	}
	chunks, err := chunkLogs([]*logspb.LogsData{batch}, 1400)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for i, ch := range chunks {
		if len(ch.Body) > 1400 {
			t.Errorf("chunk %d size %d > 1400", i, len(ch.Body))
		}
	}

	// Single record exceeding cap must fail permanently.
	oversized := &logspb.LogsData{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{makeRec(2000)}}},
		}},
	}
	_, err = chunkLogs([]*logspb.LogsData{oversized}, 1400)
	if _, ok := errors.AsType[*PermanentError](err); !ok {
		t.Fatalf("oversized record error = %v, want *PermanentError", err)
	}
}

func TestTransportPacingAt2MiBPerSecAndSerialFlight(t *testing.T) {
	t.Parallel()
	var inFlight, maxInFlight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		if r.Header.Get(sendas.ProjectHeader) != "team-1" {
			t.Errorf("missing %s header", sendas.ProjectHeader)
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	tr := &Transport{
		Version:     "test",
		HTTP:        srv.Client(),
		SignIn:      &fakeSignIn{account: "acc1", tokens: []string{"tok-1"}},
		BytesPerSec: TargetBytesPerSec, // 2 MiB/s
		Now:         func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			clock = clock.Add(d)
			return nil
		},
	}
	pacer := tr.NewRoutePacer()
	payload := make([]byte, 1<<20) // 1 MiB -> 500ms at 2 MiB/s
	for range 3 {
		if err := pacer.Send(context.Background(), SendRequest{
			Endpoint: srv.URL, TeamID: "team-1", Account: "acc1", Body: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("max in-flight = %d, want 1", maxInFlight.Load())
	}
	if len(sleeps) != 2 || sleeps[0] != 500*time.Millisecond || sleeps[1] != 500*time.Millisecond {
		t.Fatalf("pacing sleeps = %v, want [500ms 500ms]", sleeps)
	}
}

func TestTransportBackoffOn429RetryAfterAnd5xx(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	tr := &Transport{
		Version:        "test",
		HTTP:           srv.Client(),
		SignIn:         &fakeSignIn{account: "acc1", tokens: []string{"tok-1"}},
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
		Now:            func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			clock = clock.Add(d)
			return nil
		},
	}
	pacer := tr.NewRoutePacer()
	if err := pacer.Send(context.Background(), SendRequest{
		Endpoint: srv.URL, TeamID: "team-1", Account: "acc1", Body: []byte("abc"),
	}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	// Attempt 1 (429 Retry-After: 7) -> 7s; Attempt 2 (503, backoff doubled to 2s) -> 2s.
	if len(sleeps) < 2 || sleeps[0] != 7*time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("backoff sleeps = %v, want [7s, 2s]", sleeps)
	}
}

func TestTransportPartialSuccessAndAuthResponses(t *testing.T) {
	t.Parallel()
	t.Run("partial_success_returns_permanent_error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encodePartialSuccess(3, "record too large"))
		}))
		defer srv.Close()
		tr := &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "a", tokens: []string{"t"}}}
		err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: srv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
		var perm *PermanentError
		if !errors.As(err, &perm) || perm.Rejected != 3 || !strings.Contains(perm.Reason, "record too large") {
			t.Fatalf("err = %v, want PermanentError(rejected=3, record too large)", err)
		}
	})

	t.Run("401_refreshes_once_and_retries", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer tok-fresh" {
				t.Errorf("second call auth = %q, want tok-fresh", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		si := &fakeSignIn{account: "a", tokens: []string{"tok-old", "tok-fresh"}}
		tr := &Transport{Version: "test", HTTP: srv.Client(), SignIn: si}
		if err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: srv.URL, TeamID: "p1", Account: "a", Body: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		if len(si.refused) != 1 || si.refused[0] != "tok-old" {
			t.Fatalf("refused = %v, want [tok-old]", si.refused)
		}
	})

	t.Run("403_ORG_NOT_ACCESSIBLE_does_not_retire_token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`))
		}))
		defer srv.Close()
		si := &fakeSignIn{account: "a", tokens: []string{"tok-1"}}
		tr := &Transport{Version: "test", HTTP: srv.Client(), SignIn: si}
		err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: srv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
		if _, ok := errors.AsType[*OrgAccessLostError](err); !ok {
			t.Fatalf("err = %v, want *OrgAccessLostError", err)
		}
		if len(si.retired) != 0 || len(si.refused) != 0 {
			t.Fatalf("ORG_NOT_ACCESSIBLE must not retire or refuse shared user token: retired=%v refused=%v", si.retired, si.refused)
		}
	})

	t.Run("SIGN_IN_RETIRED_marks_retired", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`))
		}))
		defer srv.Close()
		si := &fakeSignIn{account: "a", tokens: []string{"tok-1"}}
		tr := &Transport{Version: "test", HTTP: srv.Client(), SignIn: si}
		err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: srv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
		if _, ok := errors.AsType[*SignInRetiredError](err); !ok {
			t.Fatalf("err = %v, want *SignInRetiredError", err)
		}
		if len(si.retired) != 1 || si.retired[0] != "tok-1" {
			t.Fatalf("retired = %v, want [tok-1]", si.retired)
		}
	})

	t.Run("second_401_403_404_return_route_error_not_permanent", func(t *testing.T) {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("denied"))
			}))
			si := &fakeSignIn{account: "a", tokens: []string{"tok-1", "tok-2"}}
			tr := &Transport{Version: "test", HTTP: srv.Client(), SignIn: si}
			err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: srv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
			srv.Close()
			if _, ok := errors.AsType[*RouteError](err); !ok {
				t.Fatalf("HTTP %d error = %T (%v), want *RouteError", code, err, err)
			}
			if _, ok := errors.AsType[*PermanentError](err); ok {
				t.Fatalf("HTTP %d must not return *PermanentError", code)
			}
		}
	})

	t.Run("200_html_or_truncated_body_never_succeeds", func(t *testing.T) {
		htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html><body>Sign in</body></html>"))
		}))
		defer htmlSrv.Close()
		tr := &Transport{
			Version: "test", HTTP: htmlSrv.Client(), SignIn: &fakeSignIn{account: "a", tokens: []string{"t"}},
			MaxRetries: 1, InitialBackoff: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil },
		}
		err := tr.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: htmlSrv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
		if _, ok := errors.AsType[*TransientError](err); !ok {
			t.Fatalf("HTML 200 error = %T (%v), want *TransientError", err, err)
		}

		// Truncated HTTP 200 body (Content-Length mismatch -> io.ErrUnexpectedEOF) must fail, not succeed.
		truncSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x0a, 0x04})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}
		}))
		defer truncSrv.Close()
		tr2 := &Transport{
			Version: "test", HTTP: truncSrv.Client(), SignIn: &fakeSignIn{account: "a", tokens: []string{"t"}},
			MaxRetries: 1, InitialBackoff: time.Millisecond, Sleep: func(context.Context, time.Duration) error { return nil },
		}
		err = tr2.NewRoutePacer().Send(context.Background(), SendRequest{Endpoint: truncSrv.URL, TeamID: "p1", Account: "a", Body: []byte("x")})
		if _, ok := errors.AsType[*TransientError](err); !ok {
			t.Fatalf("truncated 200 error = %T (%v), want *TransientError", err, err)
		}
	})
}

// chunkLogs runs batches through a ChunkWriter and collects the chunks.
func chunkLogs(batches []*logspb.LogsData, maxBytes int) ([]Chunk, error) {
	var chunks []Chunk
	w := NewChunkWriter(maxBytes, func(ch Chunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	for _, batch := range batches {
		if err := w.Add(batch); err != nil {
			return nil, err
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return chunks, nil
}
