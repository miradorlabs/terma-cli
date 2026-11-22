package hookrun

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Unparking keeps what a parked event says of its account: a person's, or no person's (a
// server key's), stays; only one its sandboxed hook could read neither for is bound to the
// sign-in in place.
func TestUnparkingKeepsTheAccountsEventsWereCollectedUnder(t *testing.T) {
	root := hookruntest.InitRepo(t)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{GitDir: root + "/.git"}
	sandboxed := Env{}
	for _, ev := range []spool.Event{
		{Name: "a-persons", Account: "account-a"},
		{Name: "a-server-keys", NoPerson: true},
		{Name: "unread"},
	} {
		if !sandboxed.park(r, parkedEvents, ev) {
			t.Fatalf("could not park %s", ev.Name)
		}
	}
	sp.Bind("account-b")
	Env{Spool: sp, Account: "account-b"}.unpark(r)
	got := map[string]string{}
	for _, ev := range hookruntest.Spooled(t, sp) {
		got[ev.Name] = ev.Account
	}
	want := map[string]string{"a-persons": "account-a", "a-server-keys": "", "unread": "account-b"}
	for name, account := range want {
		if a, ok := got[name]; !ok || a != account {
			t.Errorf("%s unparked under %q, want %q", name, a, account)
		}
	}
}

// An event an older terma parked, from before its sign-ins were retired, stays its: no
// sign-in that unparks it later takes it. One from after is bound as before.
func TestUnparkingLeavesAnOlderTermasEventsUnbound(t *testing.T) {
	root := hookruntest.InitRepo(t)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{GitDir: root + "/.git"}
	retired := time.Now().Add(-time.Hour)
	for _, ev := range []spool.Event{
		{Name: "older", Time: retired.Add(-time.Hour)},
		{Name: "newer", Time: retired.Add(time.Minute)},
		// Bound while the sign-in could not be read (auth.AccountUnreadable): maybe another
		// person's than the one signed in now, so it keeps that account.
		{Name: "unreadable", Time: retired.Add(time.Minute), Account: unreadable},
	} {
		if !(Env{}).park(r, parkedEvents, ev) {
			t.Fatalf("could not park %s", ev.Name)
		}
	}
	sp.Bind("account-b")
	Env{Spool: sp, Account: "account-b", LegacyBefore: func() time.Time { return retired }}.unpark(r)
	got := map[string]string{}
	for _, ev := range hookruntest.Spooled(t, sp) {
		got[ev.Name] = ev.Account
	}
	if got["older"] != "" || got["newer"] != "account-b" || got["unreadable"] != unreadable {
		t.Fatalf("accounts = %v; want the older terma's unbound, the newer bound, the unreadable kept", got)
	}
}

// An event bound while the sign-in could not be read is never unparked as no one's: under a
// server key it keeps its account, and ages out.
func TestAnUnreadableEventIsNeverUnparkedAsNoOnes(t *testing.T) {
	root := hookruntest.InitRepo(t)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &Repo{GitDir: root + "/.git"}
	if !(Env{}).park(r, parkedEvents, spool.Event{Name: "unreadable", Time: time.Now(), Account: unreadable}) {
		t.Fatal("could not park")
	}
	sp.BindNoPerson()
	Env{Spool: sp, ServerKey: true}.unpark(r)
	evs := hookruntest.Spooled(t, sp)
	if len(evs) != 1 || evs[0].Account != unreadable || evs[0].NoPerson {
		t.Fatalf("unparked %+v; want it kept as the unreadable sign-in's", evs)
	}
}

// A push a sandboxed pre-push parked, unable to read the sign-in, is bound as an unparked
// event is, before it is reported.
func TestAParkedPushIsBoundWhenUnparked(t *testing.T) {
	root := hookruntest.InitRepo(t)
	state := t.TempDir()
	r := &Repo{GitDir: root + "/.git"}
	rec := pushRecord{ID: "p1", Root: root, GitDir: root + "/.git", Event: spool.Event{Name: "push", Time: time.Now()}}
	if !(Env{}).park(r, parkedPushes, rec) {
		t.Fatal("could not park the push")
	}
	var recorded []string
	Env{StateDir: state, Account: "account-b", AwaitPush: func(path string) { recorded = append(recorded, path) }}.unparkPushes(r)
	if len(recorded) != 1 {
		t.Fatalf("recorded %v", recorded)
	}
	var got pushRecord
	data, err := os.ReadFile(recorded[0])
	if err != nil || json.Unmarshal(data, &got) != nil || got.Event.Account != "account-b" {
		t.Fatalf("recorded push = %+v, %v; want it bound to account-b", got.Event, err)
	}
}

// unreadable is auth.AccountUnreadable, which the hook runtime may not import.
const unreadable = "unreadable"
