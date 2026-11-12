package spool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAppendAndFlush(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := s.Append(Event{Name: "session.start", SessionID: "s1", Attrs: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _ := s.Pending(); n != 5 {
		t.Fatalf("pending %d, want 5", n)
	}

	var batches [][]Event
	sender := SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		batches = append(batches, events)
		return nil, nil
	})
	res := s.Flush(context.Background(), sender, FlushOptions{Batch: 2})
	if res.Err != nil || res.Sent != 5 || res.Dropped != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(batches) != 3 || len(batches[0]) != 2 || len(batches[2]) != 1 {
		t.Fatalf("unexpected batching: %d batches", len(batches))
	}
	if batches[0][0].Attrs["i"].(float64) != 0 || batches[2][0].Attrs["i"].(float64) != 4 {
		t.Fatal("events delivered out of order")
	}
	if n, size, _ := s.Pending(); n != 0 || size != 0 {
		t.Fatalf("spool not drained: %d events, %d bytes", n, size)
	}
}

func TestFlushKeepsEventsOnFailureAndBacksOff(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "a"})
	_ = s.Append(Event{Name: "b"})
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	boom := errors.New("collector down")
	fail := SenderFunc(func(context.Context, []Event) ([]Event, error) { return nil, boom })

	res := s.Flush(context.Background(), fail, FlushOptions{Now: now})
	if !errors.Is(res.Err, boom) || res.Sent != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if n, _, _ := s.Pending(); n != 2 {
		t.Fatalf("events must survive a failed send, have %d", n)
	}
	next := s.NextAttempt()
	if next.Sub(now) != 30*time.Second {
		t.Fatalf("first backoff should be 30s, got %s", next.Sub(now))
	}

	// Within the window: skipped without calling the sender.
	called := false
	res = s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) { called = true; return nil, nil }),
		FlushOptions{Now: now.Add(10 * time.Second)})
	if !res.Skipped || called {
		t.Fatalf("expected a skipped flush inside the backoff window: %+v called=%v", res, called)
	}
	// Force ignores the window.
	res = s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) { return nil, nil }),
		FlushOptions{Now: now.Add(10 * time.Second), Force: true})
	if res.Err != nil || res.Sent != 2 {
		t.Fatalf("forced flush failed: %+v", res)
	}
	if !s.NextAttempt().IsZero() {
		t.Fatal("backoff should clear after success")
	}
}

func TestBackoffDoubles(t *testing.T) {
	t.Parallel()
	s, _ := Open(t.TempDir())
	_ = s.Append(Event{Name: "a"})
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	fail := SenderFunc(func(context.Context, []Event) ([]Event, error) { return nil, errors.New("no") })
	s.Flush(context.Background(), fail, FlushOptions{Now: now})                                    // 30s
	s.Flush(context.Background(), fail, FlushOptions{Now: now.Add(31 * time.Second), Force: true}) // 60s
	if got := s.NextAttempt().Sub(now.Add(31 * time.Second)); got != 60*time.Second {
		t.Fatalf("second backoff %s, want 60s", got)
	}
}

func TestFlushSkipsTornLinesAndKeepsConcurrentAppends(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := Open(dir)
	_ = s.Append(Event{Name: "ok"})
	// A hook killed mid-write leaves garbage; the flusher must step over it.
	f, _ := os.OpenFile(filepath.Join(dir, eventsFile), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString("{not json\n")
	f.Close()
	_ = s.Append(Event{Name: "ok2"})

	sender := SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		// Simulate a hook appending while the batch is in flight.
		return nil, s.Append(Event{Name: "late"})
	})
	res := s.Flush(context.Background(), sender, FlushOptions{Batch: 10})
	if res.Err != nil || res.Sent != 2 || res.Expired != 0 || res.Pruned != 0 || res.Dropped != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if n, _, _ := s.Pending(); n != 1 {
		t.Fatalf("the late append must survive, pending=%d", n)
	}
}

// Held events requeue at the tail, are not re-sent in the same pass, and return next flush.
func TestHeldEventsRequeueWithoutLooping(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		name := "deliverable"
		if i%2 == 1 {
			name = "held"
		}
		if err := s.Append(Event{Name: name, Attrs: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	holder := SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		calls++
		var held []Event
		for _, e := range events {
			if e.Name == "held" {
				held = append(held, e)
			}
		}
		return held, nil
	})
	res := s.Flush(context.Background(), holder, FlushOptions{Batch: 2})
	if res.Err != nil || res.Sent != 3 || res.Held != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if calls != 3 {
		t.Fatalf("sender called %d times, want 3 (the re-queued events must not be chased)", calls)
	}
	left, err := s.Peek(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0].Name != "held" || left[1].Name != "held" {
		t.Fatalf("queue after flush = %+v", left)
	}
	if s.NextAttempt() != (time.Time{}) {
		t.Fatal("holding must not start a backoff")
	}
	res = s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) { return nil, nil }), FlushOptions{})
	if res.Sent != 2 || res.Held != 0 {
		t.Fatalf("second flush: %+v", res)
	}
	if n, _, _ := s.Pending(); n != 0 {
		t.Fatalf("pending after delivery = %d", n)
	}
}

// One refused destination does not keep another's events queued: the pass acknowledges
// what went out and ends as a failure without opening the spool-wide backoff.
func TestPartialDeliveryAcknowledgesWhatWentOutAndCarriesOn(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"ok", "refused", "held", "ok", "refused", "held"}
	for i, name := range names {
		if err := s.Append(Event{Name: name, Attrs: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("403 invalid key")
	calls, reported := 0, false
	sender := SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		calls++
		var held, undelivered []Event
		for _, e := range events {
			switch e.Name {
			case "held":
				held = append(held, e)
			case "refused":
				undelivered = append(undelivered, e)
			}
		}
		if len(undelivered) == 0 {
			return held, nil
		}
		// Like the router: the refusal is reported once, later batches only defer.
		partial := &PartialDelivery{Undelivered: undelivered}
		if !reported {
			partial.Err, reported = boom, true
		}
		return held, partial
	})
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	res := s.Flush(context.Background(), sender, FlushOptions{Batch: 2, Now: now})
	if !errors.Is(res.Err, boom) {
		t.Fatalf("a partial delivery must end the pass as a failure: %+v", res)
	}
	if res.Sent != 2 || res.Held != 2 || res.Failed != 2 {
		t.Fatalf("counts = sent %d held %d failed %d, want 2 2 2", res.Sent, res.Held, res.Failed)
	}
	if calls != 3 {
		t.Fatalf("sender called %d times, want 3 (every batch of the snapshot, and none of the re-queued)", calls)
	}
	left, err := s.Peek(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 4 {
		t.Fatalf("queue after flush = %+v, want the two refused and the two held", left)
	}
	for _, e := range left {
		if e.Name == "ok" {
			t.Fatalf("a delivered event stayed queued and would be sent again: %+v", left)
		}
	}
	if next := s.NextAttempt(); !next.IsZero() {
		t.Fatalf("a partial failure opened the spool-wide window (until %s): every destination would wait out the failed one's", next)
	}
	if !s.LastFlush().IsZero() {
		t.Fatal("a failed pass must not be recorded as a completed flush")
	}

	res = s.Flush(context.Background(), SenderFunc(func(context.Context, []Event) ([]Event, error) { return nil, nil }), FlushOptions{Force: true})
	if res.Err != nil || res.Sent != 4 || !s.NextAttempt().IsZero() {
		t.Fatalf("retry after the refusal cleared: %+v next=%s", res, s.NextAttempt())
	}
}

// A deferral leaves events queued and counted, with no error and no window.
func TestPartialDeliveryThatOnlyDefersIsNotAFailure(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Append(Event{Name: "a"})
	_ = s.Append(Event{Name: "b"})
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		return nil, &PartialDelivery{Undelivered: events[1:]}
	}), FlushOptions{})
	if res.Err != nil || res.Sent != 1 || res.Failed != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !s.NextAttempt().IsZero() {
		t.Fatal("a deferral opened the spool-wide window")
	}
	if left, _ := s.Peek(10); len(left) != 1 || left[0].Name != "b" {
		t.Fatalf("queue = %+v, want the deferred event", left)
	}
}

// A pass that reaches every destination clears a spool-wide window.
func TestPartialDeliveryClearsAStaleSpoolWideWindow(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.recordFailure(time.Now())
	_ = s.Append(Event{Name: "a"})
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		return nil, &PartialDelivery{Undelivered: events, Err: errors.New("403")}
	}), FlushOptions{Force: true})
	if res.Err == nil {
		t.Fatal("the failure must be reported")
	}
	if !s.NextAttempt().IsZero() {
		t.Fatal("the stale spool-wide window survived a pass that reached every destination")
	}
}

// Each destination backs off on its own: 30 seconds doubling to an hour, reset by a delivery.
func TestDestinationRetryWindows(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	if !s.RetryAt("a").IsZero() {
		t.Fatal("a destination with no failure has no window")
	}
	var waits []time.Duration
	for range 9 {
		waits = append(waits, s.DestinationFailed("a", now).Sub(now))
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	s.DestinationFailed("b", now)
	if got := s.RetryWindows(now); len(got) != 2 || !got["a"].Equal(now.Add(time.Hour)) || !got["b"].Equal(now.Add(30*time.Second)) {
		t.Fatalf("open windows = %v", got)
	}
	if got := s.RetryWindows(now.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("b's window closed after 30s; open = %v", got)
	}

	s.DestinationDelivered("a", now)
	if !s.RetryAt("a").IsZero() {
		t.Fatal("a delivery must close the window")
	}
	if got := s.DestinationFailed("a", now).Sub(now); got != 30*time.Second {
		t.Fatalf("after a delivery the next failure starts over, got %s", got)
	}
	if !s.NextAttempt().IsZero() {
		t.Fatal("a destination's window is not the spool's")
	}

	// A window closed for longer than any event can wait is forgotten.
	s.DestinationFailed("c", now.Add(MaxAge+2*time.Hour))
	if got := s.loadRetryWindows(); len(got) != 1 {
		t.Fatalf("windows closed past MaxAge were kept: %v", got)
	}
}

func TestFlushDropsEventsOlderThanMaxAge(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.Append(Event{Time: now.Add(-MaxAge - time.Hour), Name: "stale", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Time: now.Add(-time.Hour), Name: "fresh", SessionID: "new"}); err != nil {
		t.Fatal(err)
	}
	var sent []Event
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, evs []Event) ([]Event, error) {
		sent = append(sent, evs...)
		return nil, nil
	}), FlushOptions{Now: now, Force: true})
	if res.Err != nil || res.Expired != 1 || res.Sent != 1 || len(sent) != 1 || sent[0].Name != "fresh" {
		t.Fatalf("res %+v sent %+v", res, sent)
	}
	if rest, _ := s.Peek(10); len(rest) != 0 {
		t.Fatalf("stale event must leave the file with the batch: %+v", rest)
	}
}

func TestFlushMinIntervalSkipsARecentFlush(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ok := SenderFunc(func(_ context.Context, evs []Event) ([]Event, error) { return nil, nil })
	if err := s.Append(Event{Time: now, Name: "a", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if res := s.Flush(context.Background(), ok, FlushOptions{Now: now, MinInterval: 2 * time.Minute}); res.Skipped || res.Sent != 1 {
		t.Fatalf("first flush must run: %+v", res)
	}
	if err := s.Append(Event{Time: now, Name: "b", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	if res := s.Flush(context.Background(), ok, FlushOptions{Now: now.Add(30 * time.Second), MinInterval: 2 * time.Minute}); !res.Skipped {
		t.Fatalf("a flush 30s after the last must be skipped: %+v", res)
	}
	if res := s.Flush(context.Background(), ok, FlushOptions{Now: now.Add(3 * time.Minute), MinInterval: 2 * time.Minute}); res.Skipped || res.Sent != 1 {
		t.Fatalf("a flush past the interval must run: %+v", res)
	}
	if res := s.Flush(context.Background(), ok, FlushOptions{Now: now.Add(3*time.Minute + time.Second)}); res.Skipped {
		t.Fatalf("no interval means always flush: %+v", res)
	}
}

// Writable's probe never reaches a sender, changes the count, or reorders the queue.
func TestWritableLeavesTheQueueExactlyAsItWas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := Open(dir)
	for _, name := range []string{"a", "b", "c"} {
		if err := s.Append(Event{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(); err != nil {
		t.Fatalf("a writable spool must report writable: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the queue changed:\n%q\n%q", before, after)
	}
	if n, _, err := s.Pending(); err != nil || n != 3 {
		t.Fatalf("pending=%d err=%v, want 3", n, err)
	}
	got, err := s.Peek(10)
	if err != nil || len(got) != 3 {
		t.Fatalf("peek=%v err=%v", got, err)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Name != want {
			t.Fatalf("order changed: %v", got)
		}
	}
	// And the probe is not something a flusher could ever be handed.
	var sent []Event
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, evs []Event) ([]Event, error) {
		sent = append(sent, evs...)
		return nil, nil
	}), FlushOptions{})
	if res.Err != nil || res.Sent != 3 || len(sent) != 3 {
		t.Fatalf("res=%+v sent=%v", res, sent)
	}
	for _, e := range sent {
		if e.Name == probeEventName {
			t.Fatal("a probe line reached a sender")
		}
	}
}

// A spool that exists but cannot be appended to fails Writable.
func TestWritableReportsAnUnwritableSpool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := Open(dir)
	// A directory where the queue file belongs: open succeeds, writing fails.
	if err := os.Mkdir(filepath.Join(dir, eventsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(); err == nil {
		t.Fatal("a spool whose queue cannot be written must report an error")
	}
}

// Mentions finds a needle anywhere in a queued line, without decoding it.
func TestMentions(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Mentions([]string{"abc"}); err != nil || len(got) != 0 {
		t.Fatalf("empty spool: %v, %v", got, err)
	}
	if err := s.Append(Event{Name: "x", Attrs: map[string]any{"id": "abc123"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Mentions([]string{"abc123", "def456", ""})
	if err != nil || !got["abc123"] || got["def456"] || got[""] {
		t.Fatalf("Mentions = %v, %v", got, err)
	}
}

// What a sender delivered is acknowledged though the pass ran out of time sending it: the
// batch does not replay, resending what went and holding up what waits behind it.
func TestDeliveredBatchIsAcknowledgedPastTheDeadline(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "sent"}, Event{Name: "cut"}); err != nil {
		t.Fatal(err)
	}
	res := s.Flush(context.Background(), SenderFunc(func(ctx context.Context, events []Event) ([]Event, error) {
		<-ctx.Done() // the pass's deadline passes mid-send
		return nil, &PartialDelivery{Undelivered: events[1:], Err: ctx.Err()}
	}), FlushOptions{Force: true, Timeout: 20 * time.Millisecond})
	if res.Err == nil {
		t.Fatal("a pass cut short reported no error")
	}
	left, err := s.Peek(10)
	if err != nil || len(left) != 1 || left[0].Name != "cut" {
		t.Fatalf("spool holds %+v, %v; want only the undelivered event", left, err)
	}
}

// A stall opens a window as a failure does, doubling it, and says so; a failure after it
// no longer does.
func TestDestinationStalled(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first := s.DestinationStalled("p1", now)
	if second := s.DestinationStalled("p1", now); second.Sub(first) != minBackoff {
		t.Fatalf("a second stall's window ends %v after the first's, want %v", second.Sub(first), minBackoff)
	}
	if _, ok := s.StalledWindows(now)["p1"]; !ok || s.RetryWindows(now)["p1"].IsZero() {
		t.Fatal("a stall's window is not listed as one")
	}
	s.DestinationFailed("p1", now)
	if _, ok := s.StalledWindows(now)["p1"]; ok {
		t.Fatal("a failure's window is listed as a stall's")
	}
}

// A drain that will not wait leaves at once while another delivers the spool.
func TestDrainNoWaitSkipsABusySpool(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	draining, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.Drain(context.Background(), DrainOptions{}, func(context.Context, []Pending) {
			close(draining)
			<-release
		})
		close(done)
	}()
	<-draining
	res := s.Drain(context.Background(), DrainOptions{NoWait: true}, func(context.Context, []Pending) {
		t.Error("a busy spool was drained")
	})
	close(release)
	<-done
	if !res.Skipped || res.Reason != SkipBusy {
		t.Fatalf("Drain = %+v", res)
	}
}

// A drain hands out what is queued, rid of what expired or cannot be read; each event
// settled leaves the queue at once, requeued ones go to its tail, the rest stay where they
// were, and what was appended meanwhile is kept.
func TestDrainSettlesEachEventOnItsOwn(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.Append(Event{Name: "a", Time: now}, Event{Name: "old", Time: now.Add(-MaxAge - time.Hour)}, Event{Name: "b", Time: now}, Event{Name: "c", Time: now}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not json\n")
	_ = f.Close()
	var settledEarly []Event
	var later [][]Pending
	res := s.Drain(context.Background(), DrainOptions{Now: now}, func(_ context.Context, events []Pending) {
		if later != nil || len(events) != 3 || events[0].Name != "a" {
			later = append(later, events)
			return
		}
		later = [][]Pending{}
		if events[1].Name != "b" || events[2].Name != "c" {
			t.Fatalf("Drain handed out %+v", events)
		}
		if err := s.Settle(events[0]); err != nil {
			t.Fatal(err)
		}
		settledEarly, _ = s.Peek(10)
		if err := s.Append(Event{Name: "new", Time: now}); err != nil {
			t.Fatal(err)
		}
		again := events[1].Event
		again.Attempts = 1
		if err := s.Settle(events[1], again); err != nil {
			t.Fatal(err)
		}
		if err := s.Settle(events[0]); !errors.Is(err, errNotQueued) {
			t.Fatalf("settling twice = %v", err)
		}
	})
	if res.Expired != 1 || res.Dropped != 1 || res.Err != nil {
		t.Fatalf("Drain = %+v", res)
	}
	// What was appended meanwhile is handed out in the same pass; what was requeued is not.
	if len(later) != 1 || len(later[0]) != 1 || later[0][0].Name != "new" {
		t.Fatalf("Drain then handed out %+v", later)
	}
	if len(settledEarly) != 2 || settledEarly[0].Name != "b" {
		t.Fatalf("after the first settle the queue held %+v", settledEarly)
	}
	left, err := s.Peek(10)
	var names []string
	for _, e := range left {
		names = append(names, e.Name)
	}
	if err != nil || strings.Join(names, ",") != "c,new,b" || left[2].Attempts != 1 {
		t.Fatalf("queue holds %v, %v", names, err)
	}
}

// Idle waits out a flusher delivering the spool.
func TestIdleWaitsForAFlusher(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	draining, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s.Drain(context.Background(), DrainOptions{}, func(context.Context, []Pending) {
			close(draining)
			time.Sleep(100 * time.Millisecond)
		})
	}()
	t.Cleanup(func() { <-done })
	<-draining
	start := time.Now()
	if err := s.Idle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("Idle returned after %v while a drain ran", waited)
	}
}

// An image record's attempts survive the queue.
func TestAttemptsSurviveTheQueue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "x", Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Peek(1); err != nil || len(got) != 1 || got[0].Attempts != 2 {
		t.Fatalf("Peek = %+v, %v", got, err)
	}
}

// Settle takes one line of two alike, and keeps what was appended while a drain ran.
func TestSettleTakesOneOfTwinLines(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	twin := Event{Name: "twin", Time: at}
	if err := s.Append(twin, Event{Name: "between", Time: at}, twin); err != nil {
		t.Fatal(err)
	}
	rounds := 0
	s.Drain(context.Background(), DrainOptions{Now: at}, func(_ context.Context, events []Pending) {
		if rounds++; rounds > 1 {
			if len(events) != 1 || events[0].Name != "appended" {
				t.Errorf("the second round handed out %+v", events)
			}
			return
		}
		if len(events) != 3 {
			t.Fatalf("Drain handed out %+v", events)
		}
		if err := s.Append(Event{Name: "appended", Time: at}); err != nil {
			t.Fatal(err)
		}
		// The second twin's line is the first's too: either settles one, the first in the queue.
		if err := s.Settle(events[2]); err != nil {
			t.Fatal(err)
		}
		if err := s.Settle(events[0]); err != nil {
			t.Fatal(err)
		}
		if err := s.Settle(events[0]); !errors.Is(err, errNotQueued) {
			t.Fatalf("a third settle of two twins = %v", err)
		}
	})
	left, err := s.Peek(10)
	var names []string
	for _, e := range left {
		names = append(names, e.Name)
	}
	if err != nil || strings.Join(names, ",") != "between,appended" {
		t.Fatalf("queue holds %v, %v", names, err)
	}
}

// A sender's mark outlives its pass.
func TestMark(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Mark() != "" {
		t.Fatal("a new spool has a mark")
	}
	s.SetMark("p2")
	if again, _ := Open(s.dir); again.Mark() != "p2" {
		t.Fatalf("mark = %q", again.Mark())
	}
}

// Values reads one attribute of each whole queued line, "" for a line without it.
func TestValues(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "a", Attrs: map[string]any{"project": "p1"}}, Event{Name: "b"}, Event{Name: "c", Attrs: map[string]any{"project": "p2"}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"name":"d","attrs":{"project":"p3"}}`) // still being written
	_ = f.Close()
	got, err := s.Values("project")
	if err != nil || len(got) != 3 || !got["p1"] || !got["p2"] || !got[""] {
		t.Fatalf("Values = %v, %v", got, err)
	}
}

// A host's Retry-After stretches the window, up to an hour.
func TestDestinationFailedUntil(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if got := s.DestinationFailedUntil("p1", now, now.Add(10*time.Minute)); got.Before(now.Add(10*time.Minute - time.Second)) {
		t.Fatalf("window ends %v after now, want 10m", got.Sub(now))
	}
	if got := s.DestinationFailedUntil("p2", now, now.Add(5*time.Hour)); got.After(now.Add(maxBackoff + time.Second)) {
		t.Fatalf("window ends %v after now, want at most an hour", got.Sub(now))
	}
	if got := s.DestinationFailedUntil("p3", now, time.Time{}); got.Sub(now) > minBackoff+time.Second {
		t.Fatalf("window with no Retry-After ends %v after now", got.Sub(now))
	}
}

// A host's Retry-After stretches one window and does not become the base the next failure
// doubles.
func TestRetryAfterIsNoBackoffBase(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.DestinationFailedUntil("p1", now, now.Add(50*time.Minute))
	if got := s.DestinationFailed("p1", now); got.Sub(now) > 2*minBackoff+time.Second {
		t.Fatalf("the next failure waits %v, want the doubled %v", got.Sub(now), 2*minBackoff)
	}
}

// A hold lasts until it ends or is taken away, and leaves retry windows alone: it neither
// opens one nor hides or stretches a failure's.
func TestHolds(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	failed := s.DestinationFailed("p1", now)
	s.Hold(now.Add(5*time.Minute), "p1", "p2")
	holds := s.Holds(now)
	if len(holds) != 2 || holds["p2"].Before(now.Add(4*time.Minute)) {
		t.Fatalf("holds = %v", holds)
	}
	if !s.RetryAt("p1").Equal(failed) || !s.RetryAt("p2").IsZero() {
		t.Fatalf("a hold moved the retry windows: p1 %v (failed until %v), p2 %v", s.RetryAt("p1"), failed, s.RetryAt("p2"))
	}
	s.Unhold("p2")
	if _, held := s.Holds(now)["p2"]; held {
		t.Fatal("a hold taken away stayed")
	}
	if len(s.Holds(now.Add(time.Hour))) != 0 {
		t.Fatal("a hold outlived its end")
	}
}

// A Drain says when lines were appended after its last read, which it did not hand out.
func TestDrainSaysItLeftWork(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.Append(Event{Name: "a", Time: at}, Event{Name: "b", Time: at}); err != nil {
		t.Fatal(err)
	}
	quiet := s.Drain(context.Background(), DrainOptions{Now: at}, func(_ context.Context, events []Pending) {
		again := events[0].Event
		again.Attempts = 1
		_ = s.Settle(events[0], again)
	})
	if quiet.Grew {
		t.Fatal("a drain with nothing appended says it left work")
	}
	drainEnding = func() { _ = s.Append(Event{Name: "late", Time: at}) }
	t.Cleanup(func() { drainEnding = func() {} })
	if res := s.Drain(context.Background(), DrainOptions{Now: at}, func(context.Context, []Pending) {}); !res.Grew {
		t.Fatal("a drain that missed an append says nothing")
	}
}

// Remove takes several events out in one rewrite, and none when one is gone.
func TestRemove(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.Append(Event{Name: "a", Time: at}, Event{Name: "b", Time: at}, Event{Name: "c", Time: at}); err != nil {
		t.Fatal(err)
	}
	s.Drain(context.Background(), DrainOptions{Now: at}, func(_ context.Context, events []Pending) {
		if err := s.Remove(events[0], events[2]); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(events[1], events[0]); !errors.Is(err, errNotQueued) {
			t.Fatalf("removing a gone event = %v", err)
		}
	})
	if left, err := s.Peek(10); err != nil || len(left) != 1 || left[0].Name != "b" {
		t.Fatalf("queue holds %+v, %v", left, err)
	}
}
