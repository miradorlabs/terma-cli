package spool

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// Routed destination metadata (Profile, Enrollment, Run, OwnLocation) round-trips
// alongside the #152 lane tags (Account / NoPerson) in events-v2.jsonl, while untagged
// events still drop and count in spool/losses/ even when destination fields are set.
func TestDestinationMetadataRoundTripAndLaneAccounting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sp, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Unbound spool drops events even if they carry destination metadata, counting them as unreadable.
	if err := sp.Append(Event{
		Time:        time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		Name:        "terma.commit",
		SessionID:   "sess-untagged",
		Profile:     "work",
		Enrollment:  "enr-1",
		Run:         "run-1",
		OwnLocation: true,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := sp.Pending(); err != nil || n != 0 {
		t.Fatalf("unbound spool queued %d events (err=%v), want 0", n, err)
	}
	losses, err := ReadLosses(dir, time.Now())
	if err != nil || losses.Counts[sendas.LostUnreadable] != 1 {
		t.Fatalf("losses = %+v (err=%v), want unreadable=1", losses, err)
	}

	// Account-bound event preserves all destination fields alongside Account.
	const acct = "0123456789abcdef"
	sp.Bind(acct)
	want := Event{
		Time:        time.Date(2026, 11, 20, 12, 5, 0, 0, time.UTC),
		Name:        "terma.files_touched",
		SessionID:   "sess-1",
		Repository:  config.Repository{Origin: "github.com/acme/api"},
		Profile:     "work",
		Enrollment:  "enr-acme",
		Run:         "run-42",
		OwnLocation: true,
	}
	if err := sp.Append(want); err != nil {
		t.Fatal(err)
	}

	// NoPerson event preserves destination fields alongside NoPerson.
	sp.BindNoPerson()
	wantNoPerson := Event{
		Time:       time.Date(2026, 11, 20, 12, 6, 0, 0, time.UTC),
		Name:       "terma.session.title",
		SessionID:  "sess-2",
		Profile:    "default",
		Enrollment: "enr-server",
		Run:        "run-99",
	}
	if err := sp.Append(wantNoPerson); err != nil {
		t.Fatal(err)
	}

	got, err := sp.Peek(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("Peek = %d events, err=%v", len(got), err)
	}
	if got[0].Account != acct || got[0].NoPerson || got[0].Profile != "work" ||
		got[0].Enrollment != "enr-acme" ||
		got[0].Run != "run-42" || !got[0].OwnLocation {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Account != "" || !got[1].NoPerson || got[1].Profile != "default" ||
		got[1].Enrollment != "enr-server" || got[1].Run != "run-99" || got[1].OwnLocation {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

// Sessions scans every queued event's SessionID in events-v2.jsonl without taking the
// append lock, skips blank session IDs and corrupt lines, handles an absent queue, and
// stops on context cancellation.
func TestSessionsScan(t *testing.T) {
	t.Parallel()
	sp, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Absent queue returns nil with no callbacks.
	seen := map[string]int{}
	if err := sp.Sessions(t.Context(), func(id string) { seen[id]++ }); err != nil || len(seen) != 0 {
		t.Fatalf("empty spool Sessions: seen=%v err=%v", seen, err)
	}

	sp.Bind("0123456789abcdef")
	if err := sp.Append(
		Event{Name: "e1", SessionID: "s1"},
		Event{Name: "e2"},
		Event{Name: "e3", SessionID: "s2"},
		Event{Name: "e4", SessionID: "s1"},
	); err != nil {
		t.Fatal(err)
	}
	if err := sp.Sessions(t.Context(), func(id string) { seen[id]++ }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen["s1"] != 2 || seen["s2"] != 1 {
		t.Fatalf("seen = %v, want s1:2 s2:1", seen)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sp.Sessions(ctx, func(string) { t.Fatal("called after cancel") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Sessions err = %v, want context.Canceled", err)
	}

	// Empty queue file yields zero callbacks and nil error.
	emptyDir := t.TempDir()
	emptySp, err := Open(emptyDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptySp.path(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var emptyCount int
	if err := emptySp.Sessions(t.Context(), func(string) { emptyCount++ }); err != nil || emptyCount != 0 {
		t.Fatalf("empty file Sessions: count=%d err=%v", emptyCount, err)
	}

	// Truncated mid-line JSON at EOF is skipped while valid preceding lines are read.
	if err := os.WriteFile(emptySp.path(), []byte(`{"session_id":"ok-1"}`+"\n"+`{"session_id":"trun`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := emptySp.Sessions(t.Context(), func(id string) { got = append(got, id) }); err != nil {
		t.Fatalf("truncated Sessions err = %v", err)
	}
	if len(got) != 1 || got[0] != "ok-1" {
		t.Fatalf("truncated Sessions got = %v, want [ok-1]", got)
	}
}
