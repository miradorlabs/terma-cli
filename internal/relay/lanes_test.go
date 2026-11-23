package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// laneRelay runs a relay on a fresh outbox, every session claimed for p1 and sent to u,
// with key as the team's server key ("" sends with signIn), that waits retry for a sign-in
// that can't be read; and serves its exports.
func laneRelay(t *testing.T, u *signInUpstream, signIn SignIn, key string, retry time.Duration) (*Relay, *httptest.Server) {
	t.Helper()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: key, IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	r.unreadableRetry = retry
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// queuedIn is how many parts r's outbox holds, on every route.
func queuedIn(r *Relay) int {
	routes, _ := r.outbox.routes()
	n := 0
	for _, rt := range routes {
		entries, _ := r.outbox.list(rt)
		n += len(entries)
	}
	return n
}

// What arrives under a server key is bound to the no_person lane, and the key sends it.
func TestAServerKeysPartLandsOnTheNoPersonRoute(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusServiceUnavailable) // so it is seen queued first
	r, srv := laneRelay(t, u, &testSignIn{account: sendas.NoPersonLane}, "server-key", time.Second)
	post(t, srv, "/v1/logs", oneLog("keyed"), "application/x-protobuf", token, false)
	if entries, _ := r.outbox.list(routeOf(sendas.NoPersonLane, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		routes, _ := r.outbox.routes()
		t.Fatalf("the no_person lane holds %d parts; routes %v", len(entries), routes)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	if got := u.requests(); len(got) != 2 || got[1] != "Bearer server-key " {
		t.Fatalf("sent %q, want the part sent again with the key", got)
	}
}

// A route is one lane, and leaves only under its own credential: no person's only under a
// server key, a person's only under that person's sign-in, and neither a route with no lane
// nor an older terma's ambiguous backlog under either.
func TestEachLaneLeavesOnlyUnderItsOwnCredential(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, SignIn: &testSignIn{account: accountA}})
	for _, c := range []struct {
		account   string
		serverKey bool
		may       bool
	}{
		{sendas.NoPersonLane, true, true},
		{sendas.NoPersonLane, false, false},
		{accountA, false, true},
		{accountA, true, false},
		{accountB, false, false},
		{"", true, false},
		{"", false, false},
		{auth.LegacyAccountAmbiguous, true, false},
		{auth.LegacyAccountAmbiguous, false, false},
		{sendas.AccountUnreadable, false, false},
	} {
		if got := r.mayLeave(routeOf(c.account, claim.Claim{ProjectID: "p1"}), c.serverKey); got != c.may {
			t.Errorf("lane %q under a server key %v: may leave %v, want %v", c.account, c.serverKey, got, c.may)
		}
	}
	// A profile just switched to a server key, sent by a policy that names no key yet: no
	// person's lane is no sign-in's, though it is what the sign-in reads.
	keyed := newRelay(Options{Dir: t.TempDir(), Token: token, SignIn: &testSignIn{account: sendas.NoPersonLane}})
	if keyed.mayLeave(routeOf(sendas.NoPersonLane, claim.Claim{ProjectID: "p1"}), false) {
		t.Error("no person's parts may leave with no server key")
	}
}

// What a server key collected never leaves once a person signs in, not even with their
// token, and leaves again once the key is back.
func TestAPersonsSignInNeverSendsTheNoPersonRoute(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, sendas.NoPersonLane)
	signedInRelay(t, u, &testSignIn{account: accountA}, dir, "")
	time.Sleep(200 * time.Millisecond)
	if got := u.requests(); len(got) != 0 {
		t.Fatalf("sent %q: no person's part left under a person's sign-in", got)
	}
	if entries, _ := (outbox{dir: dir}).list(routeOf(sendas.NoPersonLane, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("the no_person lane holds %d parts, want its one kept", len(entries))
	}
}

// A person's profile signed out binds what arrives to no lane: it is lost as it is placed,
// counted as signed_out in the stats and where doctor reads losses, and nothing is written.
func TestASignedOutPartIsLostAndCounted(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	r, srv := laneRelay(t, u, &testSignIn{}, "", time.Second)
	post(t, srv, "/v1/logs", oneLog("signed out"), "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.signed_out.logs"] == 1 })
	if n := queuedIn(r); n != 0 {
		t.Fatalf("the outbox holds %d parts collected signed out", n)
	}
	if l, err := ReadOutboxLosses(r.outbox.dir); err != nil || l.Counts["signed_out"] != 1 || l.Last.IsZero() {
		t.Fatalf("losses = %+v, %v; want one signed_out", l, err)
	}
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d requests for a part collected signed out", n)
	}
}

// scriptedSignIn answers Account from accounts in turn, then with the last for good, and
// counts the asks.
type scriptedSignIn struct {
	testSignIn
	smu      sync.Mutex
	accounts []string
	asked    int
}

func (s *scriptedSignIn) Account() string {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.asked++
	a := s.accounts[min(s.asked, len(s.accounts))-1]
	s.set(a)
	return a
}

func (s *scriptedSignIn) asks() int {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.asked
}

// A sign-in that can't be read is asked again for a short while; one still unreadable then
// loses what arrived, counted as unreadable, and writes nothing.
func TestAnUnreadableSignInIsRetriedThenLost(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &scriptedSignIn{accounts: []string{sendas.AccountUnreadable}}
	r, srv := laneRelay(t, u, signIn, "", 300*time.Millisecond)
	start := time.Now()
	post(t, srv, "/v1/logs", oneLog("unreadable"), "application/x-protobuf", token, false)
	if waited := time.Since(start); waited < 300*time.Millisecond || waited > 3*time.Second {
		t.Fatalf("the export was answered after %v; want it to wait out the retry, and no longer", waited)
	}
	if n := signIn.asks(); n < 2 {
		t.Fatalf("the sign-in was asked %d times; want it asked again", n)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unreadable.logs"] == 1 })
	if n := queuedIn(r); n != 0 {
		t.Fatalf("the outbox holds %d parts collected while unreadable", n)
	}
	if l, err := ReadOutboxLosses(r.outbox.dir); err != nil || l.Counts["unreadable"] != 1 {
		t.Fatalf("losses = %+v, %v; want one unreadable", l, err)
	}
}

// A sign-in written as an export arrives reads as unreadable for a moment: the retry binds
// the export to the sign-in once it reads, and it leaves under that sign-in.
func TestASignInSwapMidWriteIsRecoveredByTheRetry(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &scriptedSignIn{accounts: []string{sendas.AccountUnreadable, sendas.AccountUnreadable, accountB}}
	r, srv := laneRelay(t, u, signIn, "", 3*time.Second)
	post(t, srv, "/v1/logs", oneLog("mid-swap"), "application/x-protobuf", token, false)
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountB+" p1" {
		t.Fatalf("sent %q, want B's token", got)
	}
	if c := r.Stats().Snapshot().Counters; c["dropped.unreadable.logs"] != 0 {
		t.Fatalf("counters %v: the swap was counted as a loss", c)
	}
}

// A switch with A's parts already queued: what arrives while signed out between is lost
// and counted, what arrives under B leaves under B, and A's queued parts stay A's, never
// leaving under B.
func TestASwitchKeepsTheQueuedPartsTheirCollectors(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusServiceUnavailable) // A's first send fails: A's part stays queued
	signIn := &testSignIn{account: accountA}
	r, srv := laneRelay(t, u, signIn, "", time.Second)
	post(t, srv, "/v1/logs", oneLog("A's"), "application/x-protobuf", token, false)
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	signIn.set("")
	post(t, srv, "/v1/logs", oneLog("signed out"), "application/x-protobuf", token, false)
	signIn.set(accountB)
	post(t, srv, "/v1/logs", oneLog("B's"), "application/x-protobuf", token, false)
	waitFor(t, func() bool { return len(u.requests()) == 2 })
	time.Sleep(2500 * time.Millisecond) // past A's backoff, when its sender tries again
	if got := u.requests(); !slices.Equal(got, []string{"Bearer token-" + accountA + " p1", "Bearer token-" + accountB + " p1"}) {
		t.Fatalf("sent %q; want A's failed send, then only B's own part under B", got)
	}
	if entries, _ := r.outbox.list(routeOf(accountA, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("A's lane holds %d parts, want A's one", len(entries))
	}
	if c := r.Stats().Snapshot().Counters; c["dropped.signed_out.logs"] != 1 {
		t.Fatalf("counters %v; want the signed-out part counted", c)
	}
}

// The heartbeat carries what was lost by reason, signed_out and unreadable apart.
func TestTheHeartbeatCarriesLossesByReason(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	var mu sync.Mutex
	var beats []*logspb.LogsData
	signIn := &testSignIn{}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, SignIn: signIn,
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true}, nil
		},
		HeartbeatSend: func(_ context.Context, b *logspb.LogsData) error {
			mu.Lock()
			beats = append(beats, b)
			mu.Unlock()
			return nil
		}})
	r.unreadableRetry = 10 * time.Millisecond
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("signed out"), "application/x-protobuf", token, false)
	signIn.set(sendas.AccountUnreadable)
	post(t, srv, "/v1/logs", oneLog("unreadable"), "application/x-protobuf", token, false)
	if err := r.heartbeat(t.Context(), "request", ""); err != nil {
		t.Fatal(err)
	}
	got := beatCounters(snapshot(t, &mu, &beats)[0].ResourceLogs[0].ScopeLogs[0].LogRecords[0])
	if got["dropped.signed_out.logs"] != 1 || got["dropped.unreadable.logs"] != 1 {
		t.Fatalf("beat counters %v; want one signed_out and one unreadable", got)
	}
}

// A part collected with no sign-in is never held, since it can never leave: one waiting for
// a policy that can't be read (Resolve fails, as with config.json unreadable) is lost at
// once, counted by its reason, and never pushes a genuine held part out of the hold.
func TestALostPartIsNeverHeld(t *testing.T) {
	t.Parallel()
	signIn := &scriptedSignIn{accounts: []string{accountA, sendas.AccountUnreadable}}
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
		Lookup:  func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) { return Policy{}, ErrNoKey }})
	r.unreadableRetry = 10 * time.Millisecond
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	postProto(t, srv, "/v1/logs", logsOf("genuine", 1))
	postProto(t, srv, "/v1/logs", logsOf("unreadable", maxHeld))
	r.sweep()
	r.mu.Lock()
	genuine, unreadable, heldN := len(r.held["genuine"]), len(r.held["unreadable"]), r.heldN
	r.mu.Unlock()
	if genuine != 1 || unreadable != 0 || heldN != 1 {
		t.Fatalf("held %d genuine, %d unreadable parts (%d records); want only the genuine one", genuine, unreadable, heldN)
	}
	l, err := ReadOutboxLosses(r.outbox.dir)
	if err != nil || l.Counts["unreadable"] != maxHeld || l.Counts[lostPolicyTimeout] != 0 {
		t.Fatalf("losses = %+v, %v; want %d unreadable and none held", l, err, maxHeld)
	}
}

// A sign-in that stayed unreadable through a retry is not waited out again at the next
// export, which waits a couple of polls and is lost; once it reads, the retry is back.
func TestAStillUnreadableSignInIsNotWaitedOutAgain(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &testSignIn{account: sendas.AccountUnreadable}
	const retry = time.Second
	r, srv := laneRelay(t, u, signIn, "", retry)
	post(t, srv, "/v1/logs", oneLog("first"), "application/x-protobuf", token, false)
	start := time.Now()
	post(t, srv, "/v1/logs", oneLog("second"), "application/x-protobuf", token, false)
	if waited := time.Since(start); waited >= retry/2 {
		t.Fatalf("the second export waited %v; want only a couple of polls", waited)
	}
	waitForCounters(t, r, func(c map[string]int) bool { return c["dropped.unreadable.logs"] == 2 })
	signIn.set(accountA)
	post(t, srv, "/v1/logs", oneLog("readable"), "application/x-protobuf", token, false)
	signIn.set(sendas.AccountUnreadable)
	start = time.Now()
	post(t, srv, "/v1/logs", oneLog("unreadable again"), "application/x-protobuf", token, false)
	if waited := time.Since(start); waited < retry {
		t.Fatalf("after a readable sign-in, an export waited only %v; want the retry again", waited)
	}
}

// readsAgainSignIn is a sign-in that can't be read until armed, then reads as accountA from
// the read after the next: one written as an export arrives.
type readsAgainSignIn struct {
	testSignIn
	armed bool
}

func (s *readsAgainSignIn) arm() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

func (s *readsAgainSignIn) Account() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account
	if s.armed {
		s.armed, s.account = false, accountA
	}
	return a
}

// After a failed retry, an export whose sign-in reads again a poll after it arrives is
// kept, not lost with the one before.
func TestASignInThatReadsAgainAfterAFailedRetryIsUsed(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &readsAgainSignIn{testSignIn: testSignIn{account: sendas.AccountUnreadable}}
	r, srv := laneRelay(t, u, signIn, "", 300*time.Millisecond)
	post(t, srv, "/v1/logs", oneLog("first"), "application/x-protobuf", token, false)
	signIn.arm()
	post(t, srv, "/v1/logs", oneLog("second"), "application/x-protobuf", token, false)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	if c := r.Stats().Snapshot().Counters; c["dropped.unreadable.logs"] != 1 {
		t.Fatalf("counters %v; want only the first export lost", c)
	}
}
