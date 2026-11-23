package relay

import (
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// On a server-key machine, an older relay's backlog is the key's: taken into no person's
// lane, it leaves with the key, and under a person's sign-in it never does.
func TestAServerKeyMachinesBacklogLeavesOnlyUnderTheKey(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir), "")
	r := serverKeyRelay(t, u, dir, sendas.NoPersonLane)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	if got := u.requests(); len(got) != 1 || got[0] != "Bearer server-key " {
		t.Fatalf("sent %q, want the backlog with the key", got)
	}

	u2 := newSignInUpstream(t)
	dir2 := t.TempDir()
	queue(t, legacyOutbox(dir2), "")
	r2 := signedInRelay(t, u2, &testSignIn{account: accountA}, dir2, sendas.NoPersonLane)
	r2.janitor(time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(u2.requests()); n != 0 {
		t.Fatalf("%d requests: no person's backlog left under a person's sign-in", n)
	}
	if entries, _ := (outbox{dir: dir2}).list(routeOf(sendas.NoPersonLane, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("no person's lane holds %d parts, want the older relay's one", len(entries))
	}
}

// An older relay's backlog no one sign-in may send, under an ambiguous decision, never
// leaves, neither under a sign-in nor a key, and is counted apart when it ages out.
func TestAnAmbiguousBacklogNeverLeavesAndIsCountedWhenItAgesOut(t *testing.T) {
	t.Parallel()
	for name, signIn := range map[string]bool{"a sign-in": true, "a server key": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := newSignInUpstream(t)
			dir := t.TempDir()
			queueAt(t, legacyOutbox(dir), "", time.Now().Add(-maxOutboxAge-time.Hour))
			queue(t, legacyOutbox(dir), "")
			var r *Relay
			if signIn {
				r = signedInRelay(t, u, &testSignIn{account: accountA}, dir, auth.LegacyAccountAmbiguous)
			} else {
				r = serverKeyRelay(t, u, dir, auth.LegacyAccountAmbiguous)
			}
			waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.legacy_ambiguous.logs"] == 1 })
			time.Sleep(100 * time.Millisecond)
			if n := len(u.requests()); n != 0 {
				t.Fatalf("%d requests: an ambiguous backlog left", n)
			}
			if l, err := ReadOutboxLosses(dir); err != nil || l.LegacyAmbiguous != 1 || l.Expired != 0 {
				t.Fatalf("losses = %+v, %v; want the aged-out part counted as ambiguous", l, err)
			}
			if entries, _ := (outbox{dir: dir}).list(routeOf(auth.LegacyAccountAmbiguous, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
				t.Fatalf("the ambiguous lane holds %d parts, want the recent one", len(entries))
			}
		})
	}
}

// A part an older relay still running queues after the first take is taken at the next
// sweep, into the same decision's lane: it never leaves under another.
func TestAPartAnOlderRelayQueuesLaterIsTakenByTheDecision(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	r := signedInRelay(t, u, &testSignIn{account: accountB}, dir, accountA)
	r.janitor(time.Now())
	queue(t, legacyOutbox(dir), "")
	r.janitor(time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d requests: A's backlog left under B", n)
	}
	if entries, _ := (outbox{dir: dir}).list(routeOf(accountA, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("A's lane holds %d parts, want the later one", len(entries))
	}
}
