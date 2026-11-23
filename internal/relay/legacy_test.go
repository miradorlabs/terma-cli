package relay

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
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

// An older relay named its user on every part: one taken into no person's lane on a
// server-key machine leaves with the key naming no one, and one in a person's lane names
// only that person's user, never another's.
func TestAQueuedPartNamesOnlyItsLanesUser(t *testing.T) {
	t.Parallel()
	named := func(user string) []byte {
		b, _ := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv(semconv.TermaUserIDKey, user)}},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("session.id", "old")}}}}}}}})
		return b
	}
	sentUser := func(t *testing.T, body []byte) string {
		t.Helper()
		var sent logspb.LogsData
		if err := proto.Unmarshal(body, &sent); err != nil {
			t.Fatal(err)
		}
		return attr(sent.ResourceLogs[0].GetResource().GetAttributes(), semconv.TermaUserIDKey)
	}

	u := newSignInUpstream(t)
	dir := t.TempDir()
	if err := (outbox{dir: legacyOutbox(dir)}).put(routeOf("", claim.Claim{ProjectID: "p1"}), claim.Claim{}.Repository, newEntry(time.Now(), Logs, 1), named("user-old")); err != nil {
		t.Fatal(err)
	}
	r := serverKeyRelay(t, u, dir, sendas.NoPersonLane)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	u.mu.Lock()
	body := u.sent[0]
	u.mu.Unlock()
	if got := sentUser(t, body); got != "" {
		t.Fatalf("no person's part left naming %q; want no one", got)
	}

	pol := Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a", UserAccount: accountA}
	for _, c := range []struct{ account, queued, want string }{
		{accountA, "user-a", "user-a"},
		{accountA, "user-old", "user-a"},
		{accountB, "user-a", ""},
		{sendas.NoPersonLane, "user-a", ""},
	} {
		out, _ := r.withholdQueued(Logs, named(c.queued), pol, c.account)
		if got := sentUser(t, out); got != c.want {
			t.Errorf("a part naming %q in lane %s left naming %q; want %q", c.queued, c.account, got, c.want)
		}
	}
	unnamed := pol
	unnamed.User = ""
	if out, _ := r.withholdQueued(Logs, named("user-a"), unnamed, accountA); sentUser(t, out) != "" {
		t.Errorf("under a policy that names no user, a part left naming %q", sentUser(t, out))
	}
}

// A part this relay queued in its user's own lane, naming that user, is sent as it was
// queued, never decoded: only a part naming another id, or one whose key appears anywhere
// else, is decoded and named again.
func TestAPartNamingItsOwnUserIsSentUndecoded(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	pol := Policy{IncludePrompts: true, IncludeToolContent: true, User: "user-a", UserAccount: accountA}
	queued := func(user, record string) []byte {
		p := &part{signal: Logs, account: accountA, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource:  &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("note", record)}}}}}}}}}
		stamp(p, semconv.TermaUserIDKey, user)
		b, err := proto.Marshal(p.msg)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	undecoded := func(in, out []byte) bool { return len(out) > 0 && &in[0] == &out[0] }
	for _, c := range []struct {
		name, user, record string
		fast               bool
	}{
		{"its own user", "user-a", "", true},
		{"another id", "user-old", "", false},
		{"a longer id", "user-ab", "", false},
		{"the key elsewhere too", "user-a", semconv.TermaUserIDKey, false},
	} {
		body := queued(c.user, c.record)
		out, _ := r.withholdQueued(Logs, body, pol, accountA)
		if undecoded(body, out) != c.fast {
			t.Errorf("%s: sent undecoded %v; want %v", c.name, !c.fast, c.fast)
		}
		var sent logspb.LogsData
		if err := proto.Unmarshal(out, &sent); err != nil {
			t.Fatal(err)
		}
		if got := attr(sent.ResourceLogs[0].GetResource().GetAttributes(), semconv.TermaUserIDKey); got != "user-a" {
			t.Errorf("%s: left naming %q; want user-a", c.name, got)
		}
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
