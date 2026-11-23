package spool

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// No append ever queues an event no one could send, whoever calls it: one that names
// neither an account nor no person, in a spool bound to no lane, or replayed as collected
// with none, is lost and counted by why, and the rest of the append is queued.
func TestNoAppendQueuesAnUntaggedEvent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		bind func(*Spool)
		// collected replays the events as collected (AppendAsCollected).
		collected bool
		ev        Event
		why       string
	}{
		{"never bound", func(*Spool) {}, false, Event{Name: "e"}, sendas.LostUnreadable},
		{"signed out", func(s *Spool) { s.Bind("") }, false, Event{Name: "e"}, sendas.LostSignedOut},
		{"unreadable", func(s *Spool) { s.Bind(sendas.AccountUnreadable) }, false, Event{Name: "e"}, sendas.LostUnreadable},
		{"replayed untagged", func(s *Spool) { s.Bind("aaaaaaaaaaaaaaaa") }, true, Event{Name: "e"}, sendas.LostLegacyAmbiguous},
		// A replay recorded with no lane is an older terma's, whatever the spool's own lane.
		{"replayed into a signed-out spool", func(s *Spool) { s.Bind("") }, true, Event{Name: "e"}, sendas.LostLegacyAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.bind(s)
			kept := Event{Name: "kept", Account: "bbbbbbbbbbbbbbbb"}
			appendEvents := s.Append
			if tc.collected {
				appendEvents = s.AppendAsCollected
			}
			if err := appendEvents(tc.ev, kept); err != nil {
				t.Fatal(err)
			}
			evs, err := s.Peek(10)
			if err != nil || len(evs) != 1 || evs[0].Name != "kept" {
				t.Fatalf("queued %+v, %v; want only the tagged one", evs, err)
			}
			l, err := ReadLosses(dir, time.Now())
			if err != nil || len(l.Counts) != 1 || l.Counts[tc.why] != 1 {
				t.Fatalf("losses = %v, %v; want one %s", l.Counts, err, tc.why)
			}
		})
	}
}

// Hooks counting at once lose no count: each is one append, never a read and a rewrite.
func TestConcurrentLossCountsAreAllKept(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const hooks, each = 32, 25
	var wg sync.WaitGroup
	for i := range hooks {
		wg.Go(func() {
			// Each hook opens its own spool, as each process does.
			s, err := Open(dir)
			if err != nil {
				t.Error(err)
				return
			}
			for range each {
				if err := s.CountLost(sendas.LostSignedOut, 1+i%2); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	l, err := ReadLosses(dir, time.Now())
	want := each * (hooks/2*1 + hooks/2*2)
	if err != nil || l.Counts[sendas.LostSignedOut] != want {
		t.Fatalf("counted %v, %v; want %d", l.Counts, err, want)
	}
}

// A loss is counted for LossWindow: an older one is history, and its day's file is deleted
// by the next count.
func TestALossAgesOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-LossWindow - 48*time.Hour)
	stale := filepath.Join(dir, lossesDir, old.Format(time.DateOnly)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"reason":"signed_out","count":5,"time":"` + old.Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(stale, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, _ := ReadLosses(dir, time.Now()); l.Counts[sendas.LostSignedOut] != 0 {
		t.Fatalf("an old loss still counts: %v", l.Counts)
	}
	if err := s.CountLost(sendas.LostUnreadable, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the old day is kept: %v", err)
	}
	if l, _ := ReadLosses(dir, time.Now()); l.Counts[sendas.LostUnreadable] != 2 || l.Last.IsZero() {
		t.Fatalf("losses = %+v", l)
	}
}

// What an append loses is counted once it has queued the rest: a failed append, retried,
// never counts the same events twice.
func TestAFailedAppendCountsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Bind("aaaaaaaaaaaaaaaa")
	if err := os.Mkdir(filepath.Join(dir, EventsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAsCollected(Event{Name: "untagged"}, Event{Name: "kept", Account: "aaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("the append into a directory succeeded")
	}
	if l, _ := ReadLosses(dir, time.Now()); len(l.Counts) != 0 {
		t.Fatalf("a failed append counted %v", l.Counts)
	}
}

// TakeNewLosses hands out each loss once, the day's and an imported file's alike, and what
// was counted since on the next call.
func TestTakeNewLossesHandsOutEachLossOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CountLost(sendas.LostSignedOut, 2); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(t.TempDir(), "parked")
	if err := os.WriteFile(parked, EncodeLoss(sendas.LostAmbiguousPush, 3, time.Now()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLosses(parked, "x1"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]int{
		{sendas.LostSignedOut: 2, sendas.LostAmbiguousPush: 3},
		{},
	} {
		got, err := TakeNewLosses(dir)
		if err != nil || len(got) != len(want) || got[sendas.LostSignedOut] != want[sendas.LostSignedOut] || got[sendas.LostAmbiguousPush] != want[sendas.LostAmbiguousPush] {
			t.Fatalf("call %d: %v, %v; want %v", i, got, err, want)
		}
	}
	if err := s.CountLost(sendas.LostSignedOut, 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := TakeNewLosses(dir); len(got) != 1 || got[sendas.LostSignedOut] != 1 {
		t.Fatalf("after one more: %v", got)
	}
	// Doctor still counts every one.
	if l, _ := ReadLosses(dir, time.Now()); l.Counts[sendas.LostSignedOut] != 3 || l.Counts[sendas.LostAmbiguousPush] != 3 {
		t.Fatalf("doctor's losses = %v", l.Counts)
	}
}
