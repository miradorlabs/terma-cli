package relay

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// beatReasons lists every record's heartbeat reason, in order.
func beatReasons(beats []*logspb.LogsData) []string {
	reasons := []string{}
	for _, b := range beats {
		for _, sl := range b.ResourceLogs {
			for _, sls := range sl.ScopeLogs {
				for _, rec := range sls.LogRecords {
					reasons = append(reasons, attr(rec.Attributes, semconv.TermaRelayHeartbeatReasonKey))
				}
			}
		}
	}
	return reasons
}

// beatCounters is the record's terma.relay.heartbeat.counter.<name> attributes by name,
// nil when it carries none.
func beatCounters(rec *logspb.LogRecord) map[string]int64 {
	var out map[string]int64
	for _, kv := range rec.Attributes {
		name, ok := strings.CutPrefix(kv.Key, semconv.TermaRelayHeartbeatCounterKey+".")
		if !ok {
			continue
		}
		iv, isInt := kv.Value.Value.(*commonpb.AnyValue_IntValue)
		if !isInt {
			panic("counter " + kv.Key + " is not an int")
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[name] = iv.IntValue
	}
	return out
}

// Interval beats carry the counters accrued since the last one that did, and the resource
// says what started the relay.
func TestHeartbeatCarriesCountersAndLaunch(t *testing.T) {
	t.Parallel()
	f := newFixture()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup, Now: f.clock, Version: "v9.9.9", Launch: semconv.TermaRelayLaunchHook,
		HeartbeatEvery: time.Minute,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			beats = append(beats, b)
			mu.Unlock()
			return nil
		}})
	runRelay(t, r)
	time.Sleep(20 * time.Millisecond) // Run takes its start time before the clock moves

	f.mu.Lock()
	f.now = f.now.Add(61 * time.Second)
	f.mu.Unlock()
	waitFor(t, func() bool { return len(beatReasons(snapshot(t, &mu, &beats))) >= 1 })

	// Drop something between the first and second beat, so the second has something to say.
	r.stats.dropped("logs", "unclaimed", 3)

	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	waitFor(t, func() bool { return len(beatReasons(snapshot(t, &mu, &beats))) >= 2 })

	mu.Lock()
	first := beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	second := beats[1].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	mu.Unlock()
	if got := beatCounters(first); got != nil {
		t.Errorf("the first beat already carried counters: %v", got)
	}
	// The first beat's own send is counted after it left, so the second reports it.
	want := map[string]int64{"dropped.unclaimed.logs": 3, "heartbeats_sent": 1}
	if got := beatCounters(second); !maps.Equal(got, want) {
		t.Errorf("the second beat's counters = %v, want %v", got, want)
	}
	mu.Lock()
	res := beats[1].ResourceLogs[0].Resource.Attributes
	mu.Unlock()
	if attr(res, semconv.TermaRelayLaunchKey) != semconv.TermaRelayLaunchHook {
		t.Errorf("terma.relay.launch = %q, want %q", attr(res, semconv.TermaRelayLaunchKey), semconv.TermaRelayLaunchHook)
	}
	// The counters are deltas: the next beat, with nothing else accrued, reports only the
	// second beat having been sent.
	f.mu.Lock()
	f.now = f.now.Add(time.Minute)
	f.mu.Unlock()
	waitFor(t, func() bool { return len(beatReasons(snapshot(t, &mu, &beats))) >= 3 })
	mu.Lock()
	third := beats[2].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	mu.Unlock()
	if got, want := beatCounters(third), map[string]int64{"heartbeats_sent": 1}; !maps.Equal(got, want) {
		t.Errorf("a quiet beat's counters = %v, want %v", got, want)
	}
}

// A stopping relay's last beat says why, with everything it counted since the last one.
func TestStopHeartbeat(t *testing.T) {
	t.Parallel()
	f := newFixture()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Lookup: f.lookup, Now: f.clock, Version: "v9.9.9",
		HeartbeatEvery: time.Hour,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			beats = append(beats, b)
			mu.Unlock()
			return nil
		}})
	runRelay(t, r)
	time.Sleep(20 * time.Millisecond)
	r.stats.dropped("traces", "unclaimed_overflow", 7)
	if err := r.StopHeartbeat(context.Background(), semconv.TermaRelayExitReasonIdle); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 1 {
		t.Fatalf("%d beats, want the one exit beat", len(beats))
	}
	rec := beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if reason := attr(rec.Attributes, semconv.TermaRelayHeartbeatReasonKey); reason != semconv.TermaRelayHeartbeatReasonExit {
		t.Fatalf("the exit beat's reason = %q", reason)
	}
	if exit := attr(rec.Attributes, semconv.TermaRelayExitReasonKey); exit != semconv.TermaRelayExitReasonIdle {
		t.Fatalf("terma.relay.exit.reason = %q", exit)
	}
	want := map[string]int64{"dropped.unclaimed_overflow.traces": 7}
	if got := beatCounters(rec); !maps.Equal(got, want) {
		t.Errorf("the exit beat's counters = %v, want %v", got, want)
	}
}

// A relay that sends no heartbeat refuses the exit beat too, and counts nothing.
func TestStopHeartbeatWithoutAHeartbeat(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	if err := r.StopHeartbeat(context.Background(), semconv.TermaRelayExitReasonAsked); err == nil {
		t.Fatal("a relay without HeartbeatSend accepted a stop heartbeat")
	}
}

// Counters ride the beat, not the record's body or a second record.
func TestHeartbeatCountersAreOneRecord(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		HeartbeatEvery: time.Hour,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			beats = append(beats, b)
			mu.Unlock()
			return nil
		}})
	runRelay(t, r)
	r.stats.received("logs", 5)
	if err := r.StopHeartbeat(context.Background(), semconv.TermaRelayExitReasonUpdated); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(beats) != 1 || len(beats[0].ResourceLogs) != 1 || len(beats[0].ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("the heartbeat is not one record: %+v", beats)
	}
	rec := beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(rec) != 1 {
		t.Fatalf("the heartbeat is not one record: %d", len(rec))
	}
	if got := beatCounters(rec[0]); got["received.logs"] != 5 {
		t.Errorf("counters = %v", got)
	}
}

// What is counted while a beat is on its way goes in the next beat, not in that one and not
// nowhere; and a second beat sent meanwhile does not report the first's counters again.
func TestHeartbeatCountersWhileABeatIsInFlight(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var beats []*logspb.LogsData
	inFlight, release := make(chan struct{}), make(chan struct{})
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			first := len(beats) == 0
			beats = append(beats, b)
			mu.Unlock()
			if first {
				close(inFlight)
				<-release
			}
			return nil
		}})
	r.stats.received("logs", 2)
	done := make(chan error)
	go func() {
		done <- r.heartbeat(context.Background(), semconv.TermaRelayHeartbeatReasonInterval, "")
	}()
	<-inFlight
	r.stats.received("logs", 5) // while the first beat is on its way
	if err := r.StopHeartbeat(context.Background(), semconv.TermaRelayExitReasonAsked); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := beatCounters(beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]), map[string]int64{"received.logs": 2}; !maps.Equal(got, want) {
		t.Errorf("the first beat's counters = %v, want %v", got, want)
	}
	if got, want := beatCounters(beats[1].ResourceLogs[0].ScopeLogs[0].LogRecords[0]), map[string]int64{"received.logs": 5}; !maps.Equal(got, want) {
		t.Errorf("the second beat's counters = %v, want %v", got, want)
	}
}

// A beat that fails hands its counters to the next, which reports them with the failure.
func TestHeartbeatCountersSurviveAFailedBeat(t *testing.T) {
	t.Parallel()
	var beats []*logspb.LogsData
	fail := true
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			if fail {
				return errors.New("offline")
			}
			beats = append(beats, b)
			return nil
		}})
	r.stats.received("logs", 3)
	if err := r.heartbeat(context.Background(), semconv.TermaRelayHeartbeatReasonInterval, ""); err == nil {
		t.Fatal("the failing send succeeded")
	}
	fail = false
	if err := r.heartbeat(context.Background(), semconv.TermaRelayHeartbeatReasonInterval, ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"received.logs": 3, "heartbeats_failed": 1}
	if got := beatCounters(beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]); !maps.Equal(got, want) {
		t.Errorf("counters after a failed beat = %v, want %v", got, want)
	}
}

func snapshot(t *testing.T, mu *sync.Mutex, beats *[]*logspb.LogsData) []*logspb.LogsData {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	return *beats
}

// What the content policy met unclassified is counted under each attribute's name, which an
// exporter chose; only the total leaves on the beat.
func TestHeartbeatSendsUnclassifiedNamesOnlyAsATotal(t *testing.T) {
	t.Parallel()
	var beats []*logspb.LogsData
	r := newRelay(Options{Dir: t.TempDir(), Token: token,
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error { beats = append(beats, b); return nil }})
	r.stats.unclassified("customer.secret-project", 2)
	r.stats.unclassified("resource/customer.other", 3)
	r.stats.received("logs", 4)
	if err := r.heartbeat(context.Background(), semconv.TermaRelayHeartbeatReasonInterval, ""); err != nil {
		t.Fatal(err)
	}
	rec := beats[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if got, want := beatCounters(rec), map[string]int64{"unclassified": 5, "received.logs": 4}; !maps.Equal(got, want) {
		t.Errorf("counters = %v, want %v", got, want)
	}
	for _, kv := range rec.Attributes {
		if strings.Contains(kv.Key, "customer") {
			t.Errorf("an exporter's attribute name left on the beat: %s", kv.Key)
		}
	}
}
