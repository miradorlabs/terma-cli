package relay

// Attempts to break the relay, in the spirit of PR #27's break table: floods, fuzzed
// bodies, concurrent exporters, and requests no exporter sends.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A flood of records that name no session must not grow the inbox for the half hour a
// trace may wait: over the bound, the oldest entries are placed at once.
func TestInboxBoundPlacesTheOldestEntries(t *testing.T) {
	dir := t.TempDir()
	clock := time.Unix(1790700000, 0)
	rt, routed := newTestRouter(t, dir, 30*time.Second, &clock, nil)
	rt.traceHold = 30 * time.Minute
	body := spansBody(t, map[string]any{"name": "startup", "traceId": "never-named"})
	for i := range 4 {
		if err := writeEntry(filepath.Join(dir, inboxDir), newEntry(clock.Add(time.Duration(i)*time.Second), sigTraces, formatJSON), body); err != nil {
			t.Fatal(err)
		}
	}
	rt.inboxBytes = int64(len(body)) * 2 // room for the two newest
	rt.pass(t.Context())
	if routed[machineRoute] != 2 {
		t.Fatalf("placed %d entries early, want the two oldest: %v", routed[machineRoute], routed)
	}
	left, _ := listEntries(filepath.Join(dir, inboxDir))
	if len(left) != 2 {
		t.Fatalf("inbox holds %d entries, want the two newest", len(left))
	}
}

// Whatever arrives, splitting never panics, never loses or duplicates an item, and
// every part it writes is JSON; withholding never panics either.
func FuzzSplitAndWithhold(f *testing.F) {
	f.Add([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":[{"key":"session.id","value":{"stringValue":"`+sessionA+`"}},{"key":"prompt","value":{"stringValue":"x"}}]}]}]}]}`), uint8(0))
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"t","events":[{"name":"tool.output"}]}]}]}]}`), uint8(1))
	f.Add([]byte(`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"m","sum":{"dataPoints":[{"asInt":"1"},{"asInt":"2"}]}}]}]}]}`), uint8(2))
	f.Add([]byte(`{}`), uint8(0))
	f.Add([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[null,1,"x",{}]}]}]}`), uint8(0))
	f.Fuzz(func(t *testing.T, body []byte, which uint8) {
		sig := []signal{sigLogs, sigTraces, sigMetrics}[int(which)%3]
		b, err := parseBatch(sig, body)
		if err != nil {
			return
		}
		items := b.items()
		for i, it := range items {
			it.route = fmt.Sprintf("r%d", i%3)
		}
		total := 0
		for _, route := range []string{"r0", "r1", "r2"} {
			b.withhold(route, ContentPolicy{})
			out, n, err := b.encode(route)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if n > 0 && !json.Valid(out) {
				t.Fatalf("encoded invalid JSON: %s", out)
			}
			total += n
		}
		if total != len(items) {
			t.Fatalf("split %d items into %d", len(items), total)
		}
	})
}

// Eight exporters at once, each with two sessions per request, one bound and one not:
// every record reaches its project, once.
func TestConcurrentExportersAreDeliveredExactlyOnce(t *testing.T) {
	h := newTestRelay(t)
	h.bindings[repoPath("a")] = "proj-a"
	h.keys["proj-a"] = "Bearer key-a"
	h.recordSession(sessionA, repoPath("a"))
	h.recordSession(sessionB, repoPath("unbound"))
	h.start()
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				name := fmt.Sprintf("w%d-%d", w, i)
				h.post(sigLogs, logsBody(t,
					logRecord(name+"-a", "", strAttr("session.id", sessionA)),
					logRecord(name+"-b", "", strAttr("session.id", sessionB))))
			}
		}()
	}
	wg.Wait()
	eventually(t, "every record delivered", func() bool {
		return len(h.gw.received("Bearer key-a")) == writers*each && len(h.gw.received("Bearer key-machine")) == writers*each
	})
	seen := map[string]bool{}
	for _, n := range append(h.gw.received("Bearer key-a"), h.gw.received("Bearer key-machine")...) {
		if seen[n] {
			t.Fatalf("%s delivered twice", n)
		}
		seen[n] = true
		if strings.HasSuffix(n, "-b") && !strings.Contains(strings.Join(h.gw.received("Bearer key-machine"), " "), n) {
			t.Fatalf("%s left the machine project", n)
		}
	}
}

// Requests no exporter sends are refused, and a body that is not OTLP is set aside,
// never forwarded.
func TestIntakeRefusesWhatNoExporterSends(t *testing.T) {
	h := newTestRelay(t)
	h.start()
	get, _ := http.NewRequest(http.MethodGet, h.url+"/v1/logs", nil)
	get.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	if resp, err := http.DefaultClient.Do(get); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/logs: %v %v", resp, err)
	}
	if code := h.post(signal("profiles"), []byte(`{}`)); code != http.StatusNotFound {
		t.Fatalf("an unknown signal answered %d", code)
	}
	if code := h.post(sigLogs, bytes.Repeat([]byte("x"), maxRequestBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized export answered %d", code)
	}
	if code := h.post(sigLogs, []byte(`{"resourceLogs": "not a list"}`)); code != http.StatusOK {
		t.Fatalf("a malformed export answered %d; it is accepted, then judged", code)
	}
	eventually(t, "the malformed body set aside", func() bool {
		return len(collect(filepath.Join(h.dir, deadDir))) == 1 && backlog(h.dir) == 0
	})
	if n := h.gw.requests.Load(); n != 0 {
		t.Fatalf("the gateway saw %d requests; nothing here should reach it", n)
	}
}
