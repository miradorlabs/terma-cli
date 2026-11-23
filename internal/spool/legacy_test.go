package spool

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLegacy appends lines to the older terma's queue in dir, as its hooks do.
func writeLegacy(t *testing.T, dir string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, LegacyEventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "")); err != nil {
		t.Fatal(err)
	}
}

func legacyLine(t *testing.T, e Event) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Everything this version queues goes to its own file: an older terma's flusher, which
// reads only events.jsonl, never sees it.
func TestAppendWritesOnlyTheNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := openBound(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Bind("account-a")
	if err := s.Append(Event{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, LegacyEventsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an older terma's queue was written: %v", err)
	}
	if n, _, err := s.Pending(); err != nil || n != 1 {
		t.Fatalf("pending = %d, %v; want the event in the new queue", n, err)
	}
}

// Ingesting takes an older terma's lines into the queue, tagged by the decision: untagged
// ones take it, tagged ones keep theirs, an unreadable one moves to be counted, and a line
// not yet whole stays. Taken again, nothing is taken twice; a line the older terma queues
// meanwhile is taken next time.
func TestIngestLegacyMovesEachLineOnceTaggedByTheDecision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := openBound(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeLegacy(t, dir,
		legacyLine(t, Event{Name: "untagged", Time: now}),
		legacyLine(t, Event{Name: "tagged", Time: now, Account: "account-b"}),
		legacyLine(t, Event{Name: "keys", Time: now, NoPerson: true}),
		"malformed\n",
		`{"name":"torn"`)
	if err := s.IngestLegacy("account-a", false); err != nil {
		t.Fatal(err)
	}
	if err := s.IngestLegacy("account-a", false); err != nil {
		t.Fatal(err)
	}
	// The older terma's hook, still running, finishes its line and queues another.
	writeLegacy(t, dir, `,"time":"`+now.Format(time.RFC3339Nano)+`"}`+"\n", legacyLine(t, Event{Name: "later", Time: now}))
	if err := s.IngestLegacy("account-a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, LegacyEventsFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the older queue outlived taking all of it: %v", err)
	}
	got := map[string]Event{}
	res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
		for _, e := range events {
			if _, dup := got[e.Name]; dup {
				t.Errorf("%s taken twice", e.Name)
			}
			got[e.Name] = e
		}
		return nil, nil
	}), FlushOptions{})
	if res.Err != nil || res.Dropped != 1 {
		t.Fatalf("flush = %+v; want the unreadable line counted", res)
	}
	want := map[string]string{"untagged": "account-a", "tagged": "account-b", "keys": "", "torn": "account-a", "later": "account-a"}
	for name, account := range want {
		if e, ok := got[name]; !ok || e.Account != account {
			t.Errorf("%s = %+v (%v); want account %q", name, e, ok, account)
		}
	}
	if !got["keys"].NoPerson || got["untagged"].NoPerson {
		t.Errorf("no person's = %v, untagged's = %v; want only the server key's", got["keys"].NoPerson, got["untagged"].NoPerson)
	}
}

// Under a server key's decision the older terma's lines are no person's; under an
// ambiguous one they stay untagged, and one that ages out is counted apart.
func TestIngestLegacyNoPersonAndAmbiguous(t *testing.T) {
	t.Parallel()
	for name, noPerson := range map[string]bool{"no person": true, "ambiguous": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := openBound(dir)
			if err != nil {
				t.Fatal(err)
			}
			writeLegacy(t, dir, legacyLine(t, Event{Name: "old", Time: time.Now().Add(-2 * MaxAge)}), legacyLine(t, Event{Name: "recent", Time: time.Now()}))
			if err := s.IngestLegacy("", noPerson); err != nil {
				t.Fatal(err)
			}
			var sent []Event
			res := s.Flush(context.Background(), SenderFunc(func(_ context.Context, events []Event) ([]Event, error) {
				sent = append(sent, events...)
				return nil, nil
			}), FlushOptions{})
			if len(sent) != 1 || sent[0].Name != "recent" || sent[0].NoPerson != noPerson || sent[0].Account != "" {
				t.Fatalf("handed on %+v", sent)
			}
			if noPerson && (res.Expired != 1 || res.LegacyAmbiguous != 0) || !noPerson && (res.LegacyAmbiguous != 1 || res.Expired != 0) {
				t.Fatalf("flush = %+v", res)
			}
		})
	}
}
