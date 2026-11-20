package hookrun

import (
	"testing"

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
