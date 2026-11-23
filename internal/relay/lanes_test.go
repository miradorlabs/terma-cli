package relay

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

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
// nor one of a lane it doesn't know under either.
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
		{"ambiguous", true, false},
		{"ambiguous", false, false},
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
	signedInRelay(t, u, &testSignIn{account: accountA}, dir)
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
	// Once as the relay starts, then at the export and again at its retry.
	if n := signIn.asks(); n < 3 {
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

// A relay that has read no lane yet, since its sign-in could not be read when it started,
// takes the lane its first export's retry reads (arrivalLane): a sign-in written as that
// export arrives binds it to the sign-in once it reads, and it leaves under that sign-in.
func TestAFirstExportBeforeAnyLaneTakesTheLaneItsRetryReads(t *testing.T) {
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
	// The relay reads the sign-in once as it starts, then once per export.
	signIn := &scriptedSignIn{accounts: []string{accountA, accountA, sendas.AccountUnreadable}}
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

// rewrittenSignIn is a sign-in being rewritten: unreadable until a moment after rewrite,
// whoever reads it, then as the account written.
type rewrittenSignIn struct {
	testSignIn
	until  time.Time
	misses int // reads while unreadable
}

func (s *rewrittenSignIn) rewrite(account string, takes time.Duration) {
	s.mu.Lock()
	s.account, s.until = account, time.Now().Add(takes)
	s.mu.Unlock()
}

func (s *rewrittenSignIn) missed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.misses
}

func (s *rewrittenSignIn) Account() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Now().Before(s.until) {
		s.misses++
		return sendas.AccountUnreadable
	}
	return s.account
}

// A retry ends only on the lane read last: the same person's sign-in rewritten as an
// export arrives (a token rotation) keeps the export in that person's lane, but a switch
// to another person, or to a server key, loses it as unreadable, never in the new lane.
func TestARetryEndsOnlyOnTheLaneReadLast(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, after string
		kept        bool
	}{
		{"rotation", accountA, true},
		{"another person", accountB, false},
		{"a server key", sendas.NoPersonLane, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			signIn := &rewrittenSignIn{testSignIn: testSignIn{account: accountA}}
			r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
				Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
				Resolve: func(claim.Claim) (Policy, error) {
					return Policy{Endpoint: "http://127.0.0.1:1", IncludePrompts: true, IncludeToolContent: true}, nil
				}})
			r.unreadableRetry = time.Second
			srv := httptest.NewServer(r.Handler())
			defer srv.Close()
			postProto(t, srv, "/v1/logs", logsOf("first", 1))
			// The second export reads the sign-in mid-write, then, at its retry, as c.after.
			signIn.rewrite(c.after, 5*unreadablePoll)
			start := time.Now()
			postProto(t, srv, "/v1/logs", logsOf("second", 1))
			if waited := time.Since(start); waited < unreadablePoll {
				t.Fatalf("the second export waited %v; want it read mid-write, then again", waited)
			}
			r.sweep()
			inA, _ := r.outbox.list(routeOf(accountA, claim.Claim{ProjectID: "p1"}))
			inAfter, _ := r.outbox.list(routeOf(c.after, claim.Claim{ProjectID: "p1"}))
			l, err := ReadOutboxLosses(r.outbox.dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.kept {
				if len(inA) != 2 || l.Counts["unreadable"] != 0 {
					t.Fatalf("A's lane holds %d parts, %d lost; want both kept in it", len(inA), l.Counts["unreadable"])
				}
				return
			}
			if len(inA) != 1 || len(inAfter) != 0 || l.Counts["unreadable"] != 1 {
				t.Fatalf("A's lane holds %d parts, the new lane %d, %d lost unreadable; want the second lost, in neither", len(inA), len(inAfter), l.Counts["unreadable"])
			}
		})
	}
}

// rewriteRelay is a relay whose every session is claimed for p1, its sign-in signIn, that
// waits a second for one that can't be read; it is not run, so nothing leaves.
func rewriteRelay(t *testing.T, signIn SignIn) (*Relay, *httptest.Server) {
	t.Helper()
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: "http://127.0.0.1:1", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	r.unreadableRetry = time.Second
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// laneParts is how many parts r's outbox holds in account's lane for p1, and how many it
// lost as unreadable.
func laneParts(t *testing.T, r *Relay, account string) (queued, unreadable int) {
	t.Helper()
	r.sweep()
	entries, _ := r.outbox.list(routeOf(account, claim.Claim{ProjectID: "p1"}))
	l, err := ReadOutboxLosses(r.outbox.dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries), l.Counts["unreadable"]
}

// A retry ends only on the lane in place when its export arrived: another export that reads
// B while it waits moves the lane read last to B, and says nothing of whose the waiting
// export is, so it is lost, while the other is B's.
func TestARetryIsNotSettledByAnotherExportsRead(t *testing.T) {
	t.Parallel()
	signIn := &rewrittenSignIn{testSignIn: testSignIn{account: accountA}}
	r, srv := rewriteRelay(t, signIn)
	signIn.rewrite(accountB, time.Hour)
	done := exportAsync(srv, logsOf("first", 1))
	waitFor(t, func() bool { return signIn.missed() > 0 })
	// B is written whole before the first export's next poll, and a second export reads it.
	signIn.rewrite(accountB, 0)
	postProto(t, srv, "/v1/logs", logsOf("second", 1))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	inA, _ := laneParts(t, r, accountA)
	inB, unreadable := laneParts(t, r, accountB)
	if inA != 0 || inB != 1 || unreadable != 1 {
		t.Fatalf("A's lane holds %d, B's %d, %d lost unreadable; want the first lost and the second B's", inA, inB, unreadable)
	}
}

// A switch from A to B and back to A while an export's retry waits, seen by another export
// that read B, loses the waiting export, though its retry reads A again: it may be B's.
func TestASwitchAndBackWithinARetryLosesTheExport(t *testing.T) {
	t.Parallel()
	signIn := &rewrittenSignIn{testSignIn: testSignIn{account: accountA}}
	r, srv := rewriteRelay(t, signIn)
	signIn.rewrite(accountB, time.Hour)
	done := exportAsync(srv, logsOf("first", 1))
	waitFor(t, func() bool { return signIn.missed() > 0 })
	signIn.rewrite(accountB, 0)
	postProto(t, srv, "/v1/logs", logsOf("second", 1))
	// Back to A before the first export's next poll.
	signIn.rewrite(accountA, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	inA, _ := laneParts(t, r, accountA)
	inB, unreadable := laneParts(t, r, accountB)
	if inA != 0 || inB != 1 || unreadable != 1 {
		t.Fatalf("A's lane holds %d, B's %d, %d lost unreadable; want the first lost and the second B's", inA, inB, unreadable)
	}
}

// Two other exports that read B, then A, while an export's retry waits leave the lane A's
// as it was, but changed twice: the waiting export is lost, though its retry reads A.
func TestASwitchAndBackSeenByTwoExportsLosesTheWaitingOne(t *testing.T) {
	t.Parallel()
	signIn := &rewrittenSignIn{testSignIn: testSignIn{account: accountA}}
	r, srv := rewriteRelay(t, signIn)
	signIn.rewrite(accountB, time.Hour)
	done := exportAsync(srv, logsOf("first", 1))
	waitFor(t, func() bool { return signIn.missed() > 0 })
	signIn.rewrite(accountB, 0)
	postProto(t, srv, "/v1/logs", logsOf("second", 1))
	signIn.rewrite(accountA, 0)
	postProto(t, srv, "/v1/logs", logsOf("third", 1))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	inA, _ := laneParts(t, r, accountA)
	inB, unreadable := laneParts(t, r, accountB)
	if inA != 1 || inB != 1 || unreadable != 1 {
		t.Fatalf("A's lane holds %d, B's %d, %d lost unreadable; want the second B's, the third A's, the first lost", inA, inB, unreadable)
	}
}

// Reads of the sign-in that finish out of order never move the lane back: a read that
// started before one already applied is stale, and leaves the lane and its version alone.
func TestAStaleReadNeverMovesTheLaneBack(t *testing.T) {
	t.Parallel()
	var l laneState
	older, newer := l.take(), l.take()
	l.apply(newer, accountB)
	l.apply(older, accountA)
	if last, gen := l.snapshot(); last != accountB || gen != 1 {
		t.Fatalf("lane %q at version %d; want B's, changed once", last, gen)
	}
}

// exportAsync posts m to srv's logs endpoint in the background, sending on the channel
// returned its error, or nil once it is answered 200.
func exportAsync(srv *httptest.Server, m proto.Message) <-chan error {
	done := make(chan error, 1)
	go func() {
		body, err := proto.Marshal(m)
		if err != nil {
			done <- err
			return
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/logs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			done <- err
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			done <- fmt.Errorf("export answered %d", resp.StatusCode)
			return
		}
		done <- nil
	}()
	return done
}

// A relay knows the lane in place as it starts: its first export, met by a switch to B, is
// lost, never B's; one started while the sign-in can't be read knows none, and takes B.
func TestARelayStartsKnowingItsLane(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, at string
		lost     bool
	}{
		{"signed in as A", accountA, true},
		{"unreadable", sendas.AccountUnreadable, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			signIn := &rewrittenSignIn{testSignIn: testSignIn{account: c.at}}
			r, srv := rewriteRelay(t, signIn)
			signIn.rewrite(accountB, 2*unreadablePoll)
			postProto(t, srv, "/v1/logs", logsOf("first", 1))
			inB, unreadable := laneParts(t, r, accountB)
			if c.lost != (inB == 0 && unreadable == 1) || !c.lost && inB != 1 {
				t.Fatalf("B's lane holds %d, %d lost unreadable; want lost %v", inB, unreadable, c.lost)
			}
		})
	}
}
