package relay

import (
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"google.golang.org/protobuf/proto"
)

func names(es []entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.name
	}
	return out
}

// A backed-up route sends each signal's parts in arrival order, merged across the other
// signals' parts between them, starting with the signal whose oldest part is oldest.
func TestSenderQueueMergesEachSignalInArrivalOrder(t *testing.T) {
	s := &sender{queue: map[Signal][]entry{}}
	at := time.Unix(1_800_000_000, 0)
	var logs, traces []entry
	for i := range 6 {
		sig := Logs
		if i%2 == 1 {
			sig = Traces
		}
		e := newEntry(at.Add(time.Duration(i)), sig, 1)
		s.push(e)
		if sig == Logs {
			logs = append(logs, e)
		} else {
			traces = append(traces, e)
		}
	}
	if got := s.next(maxMergeFiles); !slices.Equal(names(got), names(logs)) {
		t.Fatalf("first batch %v, want every log in order %v", names(got), names(logs))
	}
	if got := s.next(2); !slices.Equal(names(got), names(logs[:2])) {
		t.Fatalf("a batch of two: %v", names(got))
	}
	s.forget(logs)
	if got := s.next(maxMergeFiles); !slices.Equal(names(got), names(traces)) {
		t.Fatalf("after the logs left: %v, want %v", names(got), names(traces))
	}
	// The janitor removes parts anywhere in a queue, not only at its head.
	s.forget(traces[1:2])
	if got := s.next(maxMergeFiles); !slices.Equal(names(got), []string{traces[0].name, traces[2].name}) {
		t.Fatalf("after one trace expired: %v", names(got))
	}
	s.forget(slices.Concat(traces, logs))
	if s.busy() || len(s.next(maxMergeFiles)) != 0 {
		t.Fatalf("drained queue still holds %v", s.queue)
	}
}

// Parts a previous relay left are queued once, in arrival order, even when a new export
// was queued first.
func TestSenderQueueRecoversWithoutDuplicates(t *testing.T) {
	s := &sender{queue: map[Signal][]entry{}}
	at := time.Unix(1_800_000_000, 0)
	old1, old2 := newEntry(at, Logs, 1), newEntry(at.Add(time.Second), Logs, 1)
	fresh := newEntry(at.Add(time.Minute), Logs, 1)
	s.push(fresh)
	if added := s.recover([]entry{old1, old2, fresh}); !slices.Equal(names(added), []string{old1.name, old2.name}) {
		t.Fatalf("recover added %v", names(added))
	}
	if got := s.next(maxMergeFiles); !slices.Equal(names(got), []string{old1.name, old2.name, fresh.name}) {
		t.Fatalf("recovered queue %v", names(got))
	}
}

// An export arriving while a restarted relay recovers its outbox is queued once: recovery
// waits for deliverMu, so it never lists a part enqueue has written but not yet queued.
func TestRecoverOutboxWaitsForAnEnqueueInProgress(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	c := claim.Claim{ProjectID: "p1"}
	rt := routeOf(c)
	body, _ := proto.Marshal(logsOf("S", 1))
	left := newEntry(time.Unix(1_800_000_000, 0), Logs, 1)
	if err := r.outbox.put(rt, c.Repository, left, body); err != nil {
		t.Fatal(err)
	}
	// enqueue's two steps, under deliverMu as route holds it: the part is on disk, not yet queued.
	r.deliverMu.Lock()
	fresh := newEntry(time.Unix(1_800_000_001, 0), Logs, 1)
	if err := r.outbox.put(rt, c.Repository, fresh, body); err != nil {
		t.Fatal(err)
	}
	recovered := make(chan struct{})
	go func() { r.recoverOutbox(); close(recovered) }()
	time.Sleep(50 * time.Millisecond) // recovery is waiting now, or would have listed both parts
	r.sender(rt).push(fresh)
	r.deliverMu.Unlock()
	<-recovered

	s := r.sender(rt)
	s.mu.Lock()
	queued := names(s.queue[Logs])
	s.mu.Unlock()
	if !slices.Equal(queued, []string{left.name, fresh.name}) {
		t.Fatalf("queued %v, want each part once in arrival order", queued)
	}
	if n := r.Stats().Snapshot().Counters["recovered_from_outbox"]; n != 1 {
		t.Fatalf("recovered_from_outbox = %d, want only the part left behind", n)
	}
}
