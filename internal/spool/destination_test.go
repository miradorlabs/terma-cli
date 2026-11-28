package spool

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// Routed destination metadata (Profile, Enrollment) round-trips
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
		Time:       time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		Name:       "terma.commit",
		SessionID:  "sess-untagged",
		Profile:    "work",
		Enrollment: "enr-1",
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
		Time:       time.Date(2026, 11, 20, 12, 5, 0, 0, time.UTC),
		Name:       "terma.files_touched",
		SessionID:  "sess-1",
		Repository: config.Repository{Origin: "github.com/acme/api"},
		Profile:    "work",
		Enrollment: "enr-acme",
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
	}
	if err := sp.Append(wantNoPerson); err != nil {
		t.Fatal(err)
	}

	got, err := sp.Peek(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("Peek = %d events, err=%v", len(got), err)
	}
	if got[0].Account != acct || got[0].NoPerson || got[0].Profile != "work" ||
		got[0].Enrollment != "enr-acme" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Account != "" || !got[1].NoPerson || got[1].Profile != "default" ||
		got[1].Enrollment != "enr-server" {
		t.Fatalf("got[1] = %+v", got[1])
	}
}
