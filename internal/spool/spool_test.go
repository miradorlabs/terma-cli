package spool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndFlush(t *testing.T) {
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

// TestHeldEventsRequeueWithoutLooping: a sender that cannot deliver some events
// yet hands them back; they survive at the tail, are not re-sent in the same pass,
// and are offered again on the next flush.
func TestHeldEventsRequeueWithoutLooping(t *testing.T) {
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

func TestFlushDropsEventsOlderThanMaxAge(t *testing.T) {
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

// TestWritableLeavesTheQueueExactlyAsItWas: the probe doctor writes must not reach
// a sender, change the count, or reorder what is already queued — a check that
// costs an event is worse than the failure it looks for.
func TestWritableLeavesTheQueueExactlyAsItWas(t *testing.T) {
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

// A spool that exists but cannot be appended to — a fresh install whose config dir
// is owned by another user, a read-only home — is the failure this check exists for.
func TestWritableReportsAnUnwritableSpool(t *testing.T) {
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
