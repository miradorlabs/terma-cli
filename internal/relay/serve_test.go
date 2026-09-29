package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateway is a fake OTLP ingest host: it records what each project's key delivered and
// answers with whatever the test sets for that key.
type gateway struct {
	*httptest.Server
	mu       sync.Mutex
	got      map[string][]string // authorization → event names received, in order
	requests atomic.Int64
	answer   atomic.Value // func(auth string) int
}

func (g *gateway) respond(f func(auth string) int) { g.answer.Store(f) }

func newGateway(t *testing.T) *gateway {
	g := &gateway{got: map[string][]string{}}
	g.respond(func(string) int { return http.StatusOK })
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		auth := r.Header.Get("Authorization")
		if code := g.answer.Load().(func(string) int)(auth); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		body, _ := io.ReadAll(r.Body)
		names := namesIn(body)
		g.mu.Lock()
		g.got[auth] = append(g.got[auth], names...)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(g.Close)
	return g
}

// namesIn lists a body's log record bodies, span names and metric names.
func namesIn(body []byte) []string {
	var doc struct {
		Logs []struct {
			Scopes []struct {
				Records []struct {
					Body struct {
						S string `json:"stringValue"`
					} `json:"body"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
		Spans []struct {
			Scopes []struct {
				Spans []struct {
					Name string `json:"name"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
		Metrics []struct {
			Scopes []struct {
				Metrics []struct {
					Name string `json:"name"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return []string{"<opaque>"}
	}
	var out []string
	for _, r := range doc.Logs {
		for _, s := range r.Scopes {
			for _, x := range s.Records {
				out = append(out, x.Body.S)
			}
		}
	}
	for _, r := range doc.Spans {
		for _, s := range r.Scopes {
			for _, x := range s.Spans {
				out = append(out, x.Name)
			}
		}
	}
	for _, r := range doc.Metrics {
		for _, s := range r.Scopes {
			for _, x := range s.Metrics {
				out = append(out, x.Name)
			}
		}
	}
	return out
}

func (g *gateway) received(auth string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.got[auth])
}

// testRelay runs a relay against a gateway in a temporary state directory.
type testRelay struct {
	t        *testing.T
	dir      string
	gw       *gateway
	cfg      Config
	url      string
	bindings map[string]string // directory → project
	keys     map[string]string // project → key; a missing key holds the project
	machine  atomic.Value      // string
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan error
}

func newTestRelay(t *testing.T) *testRelay {
	h := &testRelay{t: t, dir: t.TempDir(), gw: newGateway(t), cfg: Config{Token: "relay-token"},
		bindings: map[string]string{}, keys: map[string]string{}}
	h.machine.Store("machine-proj")
	h.keys["machine-proj"] = "Bearer key-machine"
	return h
}

func (h *testRelay) start() {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	h.url = "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	opts := Options{
		Config: h.cfg, Dir: h.dir, Version: "test", Listener: ln, Hold: 300 * time.Millisecond,
		Binding: func(dir string) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if p, ok := h.bindings[dir]; ok && p == "!unreadable" {
				return "", errors.New("half-written binding")
			}
			return h.bindings[dir], nil
		},
		Destination: func(route string) (string, Destination, error) {
			project := route
			if route == machineRoute {
				project, _ = h.machine.Load().(string)
			}
			h.mu.Lock()
			key, ok := h.keys[project]
			h.mu.Unlock()
			if project == "" || !ok {
				return project, Destination{}, ErrHeld
			}
			return project, Destination{Endpoint: h.gw.URL, Authorization: key}, nil
		},
		Cwd: func(_ context.Context, session, source string) string {
			if source == "codex" && session == codexID {
				return "/repos/codex-repo"
			}
			return ""
		},
		Logf: func(f string, a ...any) { h.t.Logf("relay: "+f, a...) },
	}
	go func() { h.done <- Serve(ctx, opts) }()
	h.t.Cleanup(h.stop)
}

func (h *testRelay) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("relay did not stop")
	}
	h.cancel = nil
}

func (h *testRelay) post(sig signal, body []byte, header ...string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.url+sig.path(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *testRelay) recordSession(id, dir string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Join(h.dir, sessionsDir), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, sessionsDir, id), []byte(dir+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sameSet(got, want []string) bool {
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

func TestRelayRoutesEachSessionToItsRepository(t *testing.T) {
	h := newTestRelay(t)
	h.bindings["/repos/a"] = "proj-a"
	h.bindings["/repos/codex-repo"] = "proj-codex"
	h.keys["proj-a"] = "Bearer key-a"
	h.keys["proj-codex"] = "Bearer key-codex"
	h.recordSession(sessionA, "/repos/a")         // a bound repository's hook ran
	h.recordSession(sessionB, "/elsewhere/notes") // an unbound directory
	h.start()

	// One Claude export carrying two sessions, as Claude Desktop's batches can.
	if code := h.post(sigLogs, logsBody(t,
		logRecord("a.prompt", "", strAttr("session.id", sessionA)),
		logRecord("b.prompt", "", strAttr("session.id", sessionB)),
	)); code != http.StatusOK {
		t.Fatalf("export answered %d", code)
	}
	// Codex: logs name the conversation; spans name it only through their trace; the
	// session was never announced by a hook, so its rollout places it.
	h.post(sigLogs, logsBody(t, logRecord("codex.user_prompt", "trace-1", strAttr("conversation.id", codexID))))
	spans, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{"scopeSpans": []any{map[string]any{"spans": []any{
		map[string]any{"name": "run_turn", "traceId": "trace-1", "attributes": []any{strAttr("thread.id", "17")}},
		map[string]any{"name": "startup", "traceId": "trace-unknown"},
	}}}}}})
	h.post(sigTraces, spans)
	metrics, _ := json.Marshal(map[string]any{"resourceMetrics": []any{map[string]any{"scopeMetrics": []any{map[string]any{"metrics": []any{
		map[string]any{"name": "codex.turn.token_usage", "sum": map[string]any{"dataPoints": []any{map[string]any{"asInt": "5"}}}},
	}}}}}})
	h.post(sigMetrics, metrics)

	eventually(t, "every record delivered", func() bool {
		return sameSet(h.gw.received("Bearer key-a"), []string{"a.prompt"}) &&
			sameSet(h.gw.received("Bearer key-codex"), []string{"codex.user_prompt", "run_turn"}) &&
			sameSet(h.gw.received("Bearer key-machine"), []string{"b.prompt", "startup", "codex.turn.token_usage"})
	})
}

func TestRelayHoldsAnUnplacedSessionThenUsesTheMachineProject(t *testing.T) {
	h := newTestRelay(t)
	h.bindings["/repos/a"] = "proj-a"
	h.keys["proj-a"] = "Bearer key-a"
	h.start()

	// The export arrives before the session-start hook has written its record.
	h.post(sigLogs, logsBody(t, logRecord("early", "", strAttr("session.id", sessionA))))
	time.Sleep(100 * time.Millisecond)
	h.recordSession(sessionA, "/repos/a")
	eventually(t, "the early record placed once the hook recorded the session", func() bool {
		return sameSet(h.gw.received("Bearer key-a"), []string{"early"})
	})

	// A session nothing ever places goes to the machine project when its hold closes,
	// and stays there: a hook that records it later does not move the session.
	h.post(sigLogs, logsBody(t, logRecord("orphan.1", "", strAttr("session.id", sessionB))))
	eventually(t, "the orphan in the machine project", func() bool {
		return sameSet(h.gw.received("Bearer key-machine"), []string{"orphan.1"})
	})
	h.recordSession(sessionB, "/repos/a")
	h.post(sigLogs, logsBody(t, logRecord("orphan.2", "", strAttr("session.id", sessionB))))
	eventually(t, "the session kept where it was decided", func() bool {
		return sameSet(h.gw.received("Bearer key-machine"), []string{"orphan.1", "orphan.2"})
	})
	if got := h.gw.received("Bearer key-a"); !sameSet(got, []string{"early"}) {
		t.Fatalf("proj-a got %v; a decided session moved", got)
	}
}

func TestRelayKeepsEverythingThroughOutagesAndRestarts(t *testing.T) {
	h := newTestRelay(t)
	var down atomic.Bool
	down.Store(true)
	h.gw.respond(func(string) int {
		if down.Load() {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	h.start()
	h.post(sigLogs, logsBody(t, logRecord("one", "")))
	eventually(t, "a failed attempt", func() bool { return h.gw.requests.Load() > 0 })

	// Restart with the backend still down: what was accepted is on disk.
	h.stop()
	h.start()
	h.post(sigLogs, logsBody(t, logRecord("two", "")))
	down.Store(false)
	eventually(t, "both delivered after the outage", func() bool {
		return sameSet(h.gw.received("Bearer key-machine"), []string{"one", "two"})
	})
	eventually(t, "the queue drained", func() bool { return backlog(h.dir) == 0 })
}

func TestRelayHoldsAProjectWithoutAKey(t *testing.T) {
	h := newTestRelay(t)
	h.machine.Store("")
	h.start()
	h.post(sigLogs, logsBody(t, logRecord("waiting", "")))
	eventually(t, "the record routed", func() bool { return backlog(h.dir) == 1 && len(collect(filepath.Join(h.dir, inboxDir))) == 0 })
	health, err := probeAt(h)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(health.HeldProjects, machineRoute) {
		eventually(t, "the machine route reported held", func() bool {
			hl, _ := probeAt(h)
			return slices.Contains(hl.HeldProjects, machineRoute)
		})
	}
	// Setup chooses a machine project; the next retry delivers without a restart.
	h.machine.Store("machine-proj")
	h.stop()
	h.start()
	eventually(t, "delivered once a destination exists", func() bool {
		return sameSet(h.gw.received("Bearer key-machine"), []string{"waiting"})
	})
}

func TestRelayRetriesARefusedKeyAndSetsAsideAMalformedBody(t *testing.T) {
	h := newTestRelay(t)
	h.gw.respond(func(string) int { return http.StatusBadRequest })
	h.start()
	h.post(sigLogs, logsBody(t, logRecord("bad", "")))
	eventually(t, "the refused body set aside", func() bool {
		return len(collect(filepath.Join(h.dir, deadDir))) == 1 && backlog(h.dir) == 0
	})

	var refused atomic.Bool
	refused.Store(true)
	h.gw.respond(func(string) int {
		if refused.Load() {
			return http.StatusUnauthorized
		}
		return http.StatusOK
	})
	h.post(sigLogs, logsBody(t, logRecord("kept", "")))
	eventually(t, "a refused key reported", func() bool {
		hl, err := probeAt(h)
		return err == nil && strings.Contains(hl.LastError, "HTTP 401")
	})
	if n := backlog(h.dir); n != 1 {
		t.Fatalf("a refused key must keep the body queued, backlog %d", n)
	}
}

func TestIntakeAuthorizationEncodingAndProtobuf(t *testing.T) {
	h := newTestRelay(t)
	h.start()
	body := logsBody(t, logRecord("zipped", ""))
	if code := h.post(sigLogs, body, "Authorization", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token answered %d", code)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(body)
	_ = zw.Close()
	if code := h.post(sigLogs, zipped.Bytes(), "Content-Encoding", "gzip"); code != http.StatusOK {
		t.Fatalf("gzip answered %d", code)
	}
	if code := h.post(sigLogs, []byte{0x0a, 0x02, 0x08, 0x01}, "Content-Type", "application/x-protobuf"); code != http.StatusOK {
		t.Fatalf("protobuf answered %d", code)
	}
	eventually(t, "both delivered to the machine project", func() bool {
		return sameSet(h.gw.received("Bearer key-machine"), []string{"zipped", "<opaque>"})
	})
}

func TestServeRefusesASecondRelay(t *testing.T) {
	h := newTestRelay(t)
	h.start()
	eventually(t, "the first relay up", func() bool { _, err := probeAt(h); return err == nil })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer func() { _ = ln.Close() }()
	err := Serve(context.Background(), Options{Config: h.cfg, Dir: h.dir, Listener: ln,
		Binding:     func(string) (string, error) { return "", nil },
		Destination: func(string) (string, Destination, error) { return "", Destination{}, ErrHeld }})
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("second relay: %v", err)
	}
}

func probeAt(h *testRelay) (Health, error) {
	req, _ := http.NewRequest(http.MethodGet, h.url+"/healthz", nil)
	req.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	var hl Health
	err = json.NewDecoder(resp.Body).Decode(&hl)
	return hl, err
}
