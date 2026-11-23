package relay

import (
	"os"
	"path/filepath"
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
			if l, err := ReadOutboxLosses(dir); err != nil || l.Counts["legacy_ambiguous"] != 1 || l.Counts["outbox_expired"] != 0 {
				t.Fatalf("losses = %+v, %v; want the aged-out part counted as ambiguous", l, err)
			}
			if entries, _ := (outbox{dir: dir}).list(routeOf(auth.LegacyAccountAmbiguous, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
				t.Fatalf("the ambiguous lane holds %d parts, want the recent one", len(entries))
			}
		})
	}
}

// The decision takes in what an older relay held once; a part it queues after that is
// taken at the next sweep as ambiguous, which never leaves, under the decision's sign-in or
// any other: whoever is signed in since may be someone else.
func TestAPartAnOlderRelayQueuesLaterIsAmbiguous(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir), "")
	r := signedInRelay(t, u, &testSignIn{account: accountB}, dir, accountA)
	r.janitor(time.Now())
	queue(t, legacyOutbox(dir), "")
	r.janitor(time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d requests: A's backlog left under B", n)
	}
	for account, want := range map[string]int{accountA: 1, auth.LegacyAccountAmbiguous: 1} {
		if entries, _ := (outbox{dir: dir}).list(routeOf(account, claim.Claim{ProjectID: "p1"})); len(entries) != want {
			t.Errorf("%s's lane holds %d parts, want %d", account, len(entries), want)
		}
	}
}

// With no older outbox at the decision, there was nothing to take in under it: a part an
// older relay queues after is ambiguous too.
func TestAnOlderOutboxMadeAfterTheDecisionIsAmbiguous(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	r := signedInRelay(t, u, &testSignIn{account: accountB}, dir, accountA)
	r.janitor(time.Now())
	queue(t, legacyOutbox(dir), "")
	r.janitor(time.Now())
	if entries, _ := (outbox{dir: dir}).list(routeOf(auth.LegacyAccountAmbiguous, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("the ambiguous lane holds %d parts, want the later one", len(entries))
	}
}

// What an older relay held at the decision is taken in under it however long it takes: a
// route that can't be read at first waits, set aside, and is taken into the decision's lane
// once it can, never into the ambiguous one.
func TestAnUnreadableOlderRouteWaitsForTheDecision(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory its mode forbids")
	}
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir), "")
	routes, _ := (outbox{dir: legacyOutbox(dir)}).routes()
	if len(routes) != 1 {
		t.Fatalf("routes %v", routes)
	}
	route := (outbox{dir: legacyOutbox(dir)}).routeDir(routes[0])
	rel, _ := filepath.Rel(legacyOutbox(dir), route)
	parent := filepath.Dir(route)
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(legacyOutbox(dir)+legacyStagedSuffix, rel)
	t.Cleanup(func() {
		_ = os.Chmod(parent, 0o700)
		_ = os.Chmod(filepath.Dir(staged), 0o700)
	})
	r := signedInRelay(t, u, &testSignIn{account: accountB}, dir, accountA)
	r.janitor(time.Now())
	if err := os.Chmod(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	r.janitor(time.Now())
	for account, want := range map[string]int{accountA: 1, auth.LegacyAccountAmbiguous: 0} {
		if entries, _ := (outbox{dir: dir}).list(routeOf(account, claim.Claim{ProjectID: "p1"})); len(entries) != want {
			t.Errorf("%s's lane holds %d parts, want %d", account, len(entries), want)
		}
	}
}

// A run that set the older outbox aside and stopped before taking it in leaves it to the
// next, which takes it in under the decision, and what an older relay queued since as
// ambiguous.
func TestASetAsideOlderOutboxIsResumed(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir)+legacyStagedSuffix, "")
	queue(t, legacyOutbox(dir), "")
	r := signedInRelay(t, u, &testSignIn{account: accountB}, dir, accountA)
	r.janitor(time.Now())
	for account, want := range map[string]int{accountA: 1, auth.LegacyAccountAmbiguous: 1} {
		if entries, _ := (outbox{dir: dir}).list(routeOf(account, claim.Claim{ProjectID: "p1"})); len(entries) != want {
			t.Errorf("%s's lane holds %d parts, want %d", account, len(entries), want)
		}
	}
}
