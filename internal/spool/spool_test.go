package spool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
