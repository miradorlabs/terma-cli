package relay

import (
	"testing"
	"time"
)

func TestJitterSpreadsWaitsWithinBounds(t *testing.T) {
	const d = 100 * time.Second
	seen := map[time.Duration]bool{}
	for range 500 {
		got := jitter(d, 0)
		if got < 80*time.Second || got > 120*time.Second {
			t.Fatalf("jitter(%v) = %v, outside ±20%%", d, got)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct waits in 500: relays would still retry in step", len(seen))
	}
	// A gateway's Retry-After is a floor the spread never goes under.
	for range 100 {
		if got := jitter(d, d); got < d {
			t.Fatalf("jitter went under Retry-After: %v", got)
		}
	}
}

func TestBackoffBounds(t *testing.T) {
	b := time.Duration(0)
	for range 20 {
		b = nextBackoff(b, 0)
	}
	if b != maxBackoff || maxBackoff != 2*time.Minute {
		t.Fatalf("an outage settles at %v, want 2m", b)
	}
	if got := nextBackoff(0, 3*time.Minute); got != 3*time.Minute {
		t.Fatalf("a 3m Retry-After was cut to %v", got)
	}
	if got := retryAfter("86400"); got != maxRetryAfter {
		t.Fatalf("Retry-After of a day parks a project for %v", got)
	}
	r := time.Duration(0)
	for range 20 {
		r = nextBackoff(r, refusedBackoff)
	}
	if r != maxRefusedBackoff {
		t.Fatalf("a refused key settles at %v, want %v", r, maxRefusedBackoff)
	}
}

func TestPartialSuccessIsCountedNotRetried(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
	}{
		{`{"partialSuccess":{"rejectedLogRecords":"3","errorMessage":"too old"}}`, 3},
		{`{"partialSuccess":{"rejectedSpans":2}}`, 2},
		{`{"partialSuccess":{}}`, 0},
		{`{}`, 0},
		{`not json`, 0},
	} {
		if got, _ := partialSuccess([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: rejected %d, want %d", tc.body, got, tc.want)
		}
	}

	h := newTestRelay(t)
	h.gw.respondBody(`{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":"one too old"}}`)
	h.start()
	h.post(sigLogs, logsBody(t, logRecord("kept", ""), logRecord("dropped-by-gateway", "")))
	eventually(t, "the partial rejection counted once, the request not resent", func() bool {
		hl, err := probeAt(h)
		return err == nil && hl.Counters.Rejected == 1 && hl.Counters.Delivered == 1 && backlog(h.dir) == 0
	})
	if n := h.gw.requests.Load(); n != 1 {
		t.Fatalf("gateway saw %d requests; an accepted request must not be resent", n)
	}
}
