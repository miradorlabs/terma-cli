package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Behaviour probes: what a harness build does beyond a plain first turn, each found by running
// the build with its own defaults against a fixture that makes it happen (a turn resumed, a
// context near full, a provider failing, a tool printing too much). A probe keeps every request
// the build sent under its name (CaptureProbe), so refgen derives the snapshot's caching and
// behavior sections from the requests themselves, and records with RecordProbe only what no
// request shows: how the fixture was set up, and what the harness computed (a cost).
//
// TERMA_E2E_PROBES picks them: "all" (the default), "none", or a comma list of probe names, a
// name also selecting every probe it prefixes with a dash ("retries" is retries-429,
// retries-500 and retries-529; "model-alias" every alias).

// probeTimeout bounds one harness run inside a probe: a build that retries for minutes, or
// waits on something the fixture never sends, costs the night this much and no more.
const probeTimeout = 60 * time.Second

// probesSelected reports whether spec (TERMA_E2E_PROBES's value) selects any of names.
func probesSelected(spec string, names ...string) bool {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "", "all":
		return true
	case "none":
		return false
	}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, name := range names {
			if name == part || strings.HasPrefix(name, part+"-") {
				return true
			}
		}
	}
	return false
}

// probeOn reports whether this run's TERMA_E2E_PROBES selects any of names.
func probeOn(names ...string) bool {
	return probesSelected(os.Getenv("TERMA_E2E_PROBES"), names...)
}

// skipUnlessProbe skips t unless this run selects one of names.
func skipUnlessProbe(t *testing.T, names ...string) {
	t.Helper()
	if !probeOn(names...) {
		t.Skipf("TERMA_E2E_PROBES does not select %s", strings.Join(names, ", "))
	}
}

// probeSwitch records each request under the probe set last (Set), so one fixture serves a
// probe's several phases: the turns that set something up and the one that shows it.
type probeSwitch struct {
	t        *testing.T
	b        Binary
	next     http.Handler
	mu       sync.Mutex
	current  string
	handlers map[string]http.Handler
}

// captureSwitch is CaptureProbe whose probe name Set changes between requests.
func captureSwitch(t *testing.T, b Binary, probe string, next http.Handler) *probeSwitch {
	t.Helper()
	s := &probeSwitch{t: t, b: b, next: next, handlers: map[string]http.Handler{}}
	s.Set(probe)
	return s
}

// Set names the probe the requests from now on belong to.
func (s *probeSwitch) Set(probe string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlers[probe] == nil {
		s.handlers[probe] = CaptureProbe(s.t, s.b, probe, s.next)
	}
	s.current = probe
}

func (s *probeSwitch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.handlers[s.current]
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

// failingFirst answers the first n requests match accepts with status, a retry-after header of
// retryAfter seconds and body (JSON), and hands every other request to next: a provider that is
// rate limited or down for a moment.
func failingFirst(n, status, retryAfter int, body string, match func(*http.Request) bool, next http.Handler) http.Handler {
	var failed atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if match(r) && int(failed.Add(1)) <= n {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// retryProbe is what a retries probe's fixture did, for RecordProbe: refgen reads the backoff
// from the requests' arrival times, and needs to know how many were meant to fail.
type retryProbe struct {
	Status            int  `json:"status"`
	FailedFirst       int  `json:"failed_first"`
	RetryAfterSeconds int  `json:"retry_after_seconds"`
	Succeeded         bool `json:"succeeded"`
	// Error is how the run ended where it did not succeed: the harness's exit or a timeout.
	Error string `json:"error,omitempty"`
}

// toolOutputProbe is the tool-output probe's command and what it printed, so refgen can measure
// what of it the build sent back to the model.
type toolOutputProbe struct {
	Command string `json:"command"`
	Bytes   int    `json:"bytes"`
	Lines   int    `json:"lines"`
	// Line is the format of every line, %06d its number from 1: where the kept text starts and
	// ends says which part the build kept.
	Line string `json:"line"`
}

// The tool-output probes' command prints toolOutputLines numbered lines of toolOutputLine: about
// 450 KB, far over any build's limit.
const (
	toolOutputLines = 25000
	toolOutputLine  = "TERMA-LINE-%06d"
)

// largeOutput is the tool-output probes' command, with what toolOutputProbe says of it.
func largeOutput() toolOutputProbe {
	return toolOutputProbe{Command: fmt.Sprintf("seq -f 'TERMA-LINE-%%06g' 1 %d", toolOutputLines),
		Bytes: toolOutputLines * (len(fmt.Sprintf(toolOutputLine, 1)) + 1), Lines: toolOutputLines, Line: toolOutputLine}
}

// claudeUsage is a Messages API usage block: input, output, cache read, and cache writes with
// a five-minute and a one-hour lifetime.
func claudeUsage(input, output, cacheRead, write5m, write1h int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output, "cache_read_input_tokens": cacheRead,
		"cache_creation_input_tokens": write5m + write1h,
		"cache_creation":              map[string]any{"ephemeral_5m_input_tokens": write5m, "ephemeral_1h_input_tokens": write1h}}
}

// codexUsage is a Responses API usage block.
func codexUsage(input, cached, output int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details": map[string]any{"cached_tokens": cached}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}
}

// usageWriter rewrites the usage of a Responses stream's response.completed event.
type usageWriter struct {
	http.ResponseWriter
	usage map[string]any
}

func (w *usageWriter) Write(p []byte) (int, error) {
	text := string(p)
	if !strings.Contains(text, "response.completed") {
		return w.ResponseWriter.Write(p)
	}
	var out strings.Builder
	for line := range strings.Lines(text) {
		data, ok := strings.CutPrefix(line, "data: ")
		var ev map[string]any
		if ok && json.Unmarshal([]byte(data), &ev) == nil && ev["type"] == "response.completed" {
			if resp, _ := ev["response"].(map[string]any); resp != nil {
				resp["usage"] = w.usage
			}
			enc, _ := json.Marshal(ev)
			line = "data: " + string(enc) + "\n"
		}
		out.WriteString(line)
	}
	if _, err := w.ResponseWriter.Write([]byte(out.String())); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *usageWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
