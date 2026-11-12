package spool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func names(t *testing.T, s *Spool) string {
	t.Helper()
	left, err := s.Peek(10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range left {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// Read hands out what is queued, rid of what expired or cannot be read; each event settled
// leaves the queue at once, a requeued one goes to its tail, the rest stay where they were,
// and what was appended meanwhile is kept.
func TestReadAndSettleEachEventOnItsOwn(t *testing.T) {
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
	events, _, res, err := s.Read(t.Context(), now)
	if err != nil || res.Expired != 1 || res.Dropped != 1 || len(events) != 3 || events[0].Name != "a" {
		t.Fatalf("Read = %+v, %+v, %v", events, res, err)
	}
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s); got != "b,c" {
		t.Fatalf("after the first settle the queue holds %s", got)
	}
	if err := s.Append(Event{Name: "new", Time: now}); err != nil {
		t.Fatal(err)
	}
	again := events[1].Event
	again.Attempts = 1
	if err := s.Settle(events[1], again); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(events[0]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("settling twice = %v", err)
	}
	if got := names(t, s); got != "c,new,b" {
		t.Fatalf("queue holds %s", got)
	}
}

// Take keeps a second sender from an event the first is sending, and once the first
// settled it, tells the second it is gone.
func TestTakeKeepsTwoSendersApart(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "x", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := s.Read(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.Take(t.Context(), events[0])
	if err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Take(waiting, events[0]); err == nil {
		t.Fatal("a second sender took an event the first holds")
	}
	second := make(chan error, 1)
	go func() {
		release, err := s.Take(t.Context(), events[0])
		if err == nil {
			release()
		}
		second <- err
	}()
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-second; !errors.Is(err, ErrNotQueued) {
		t.Fatalf("the second sender's take = %v, want ErrNotQueued", err)
	}
}

// Settle takes one line of two alike.
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
	events, _, _, err := s.Read(t.Context(), at)
	if err != nil || len(events) != 3 {
		t.Fatalf("Read = %+v, %v", events, err)
	}
	// The second twin's line is the first's too: either settles one, the first in the queue.
	if err := s.Settle(events[2]); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(events[0]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a third settle of two twins = %v", err)
	}
	if got := names(t, s); got != "between" {
		t.Fatalf("queue holds %s", got)
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
	events, _, _, err := s.Read(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(events[0], events[2]); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(events[1], events[0]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("removing a gone event = %v", err)
	}
	if got := names(t, s); got != "b" {
		t.Fatalf("queue holds %s", got)
	}
}

// Idle waits out a sender holding an event.
func TestIdleWaitsForASender(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "x", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := s.Read(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.Take(t.Context(), events[0])
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	start := time.Now()
	if err := s.Idle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("Idle returned after %v while a sender held an event", waited)
	}
}

// A settle and an append of the same length leave the queue as long as it was, and, its
// time put back, as old: still another state of it, by the file's identity.
func TestAStampSeesASettleAndAnEqualAppend(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.Append(Event{Name: "a", Time: at}, Event{Name: "b", Time: at}); err != nil {
		t.Fatal(err)
	}
	events, stamp, _, err := s.Read(t.Context(), at)
	if err != nil || !stamp.Same(s.Stamp()) {
		t.Fatalf("Read's stamp is not the queue's: %v", err)
	}
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(); err != nil { // the rewrite
		t.Fatal(err)
	}
	if err := s.Append(Event{Name: "c", Time: at}); err != nil {
		t.Fatal(err)
	}
	// The same size and, put back, the same time: only the file's identity tells them apart.
	if err := os.Chtimes(s.path(), stamp.mod, stamp.mod); err != nil {
		t.Fatal(err)
	}
	if now := s.Stamp(); now.size != stamp.size || !now.mod.Equal(stamp.mod) || now.Same(stamp) {
		t.Fatalf("sizes %d and %d, times %v and %v; same %v", now.size, stamp.size, now.mod, stamp.mod, now.Same(stamp))
	}
}

// Senders writing windows at once lose none of each other's.
func TestWindowsWrittenAtOnceAreAllKept(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() { s.DestinationFailed(fmt.Sprint("p", i), now) })
	}
	wg.Wait()
	if n := len(s.RetryWindows(now)); n != 40 {
		t.Fatalf("%d of 40 windows kept", n)
	}
}

// Take finds a line where Read found it, after an append, and elsewhere after a rewrite;
// one settled meanwhile is not queued.
func TestTakeFindsTheLineWhereverItIs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.Append(Event{Name: "a", Time: at}, Event{Name: "b", Time: at}, Event{Name: "c", Time: at}); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := s.Read(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	take := func(p Pending) error {
		release, err := s.Take(t.Context(), p)
		if err == nil {
			release()
		}
		return err
	}
	if err := s.Append(Event{Name: "d", Time: at}); err != nil {
		t.Fatal(err)
	}
	if err := take(events[2]); err != nil {
		t.Fatalf("after an append: %v", err)
	}
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	if err := take(events[2]); err != nil {
		t.Fatalf("after a rewrite moved it: %v", err)
	}
	if err := take(events[0]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a settled event's take = %v", err)
	}
	if err := s.Settle(events[1]); err != nil {
		t.Fatal(err)
	}
	// c now sits where a stood: a line at a's offset is not a's.
	if err := take(events[0]); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a settled event at another's offset: %v", err)
	}
}

// Settling every image of a pass rewrites the queue not once: each settle appends to the
// settled log, and one compaction rewrites the queue at the end.
func TestSettlesDoNotRewriteTheQueue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for i := range 20 {
		if err := s.Append(Event{Name: fmt.Sprint("e", i), Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	events, _, _, err := s.Read(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events[:19] {
		if err := s.Settle(e); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(s.path())
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
		t.Fatal("a settle rewrote the queue")
	}
	if n, _, _ := s.Pending(); n != 1 {
		t.Fatalf("%d pending; settled lines still count", n)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s); got != "e19" {
		t.Fatalf("after the compaction the queue holds %s", got)
	}
}

// A sender killed between its settles and the compaction hands out nothing it settled
// again: the settled log outlives it, and the next Read compacts.
func TestASettledLineIsNeverHandedOutAgain(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := s.Append(Event{Name: "sent", Time: at}, Event{Name: "left", Time: at}); err != nil {
		t.Fatal(err)
	}
	events, _, _, err := s.Read(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(events[0]); err != nil {
		t.Fatal(err)
	}
	// The sender dies here; the next one opens the queue afresh.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := again.Mentions([]string{`"sent"`}); found[`"sent"`] {
		t.Fatal("a settled line is still mentioned")
	}
	events, _, _, err = again.Read(t.Context(), at)
	if err != nil || len(events) != 1 || events[0].Name != "left" {
		t.Fatalf("Read after a crash = %+v, %v", events, err)
	}
	if _, err := os.Stat(filepath.Join(dir, settledFile)); !os.IsNotExist(err) {
		t.Fatalf("the settled log outlived the compaction: %v", err)
	}
}
