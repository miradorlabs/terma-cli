package relay

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const (
	accountA = "aaaaaaaaaaaaaaaa"
	accountB = "bbbbbbbbbbbbbbbb"
)

// testSignIn is a sign-in that can be switched to another account while the relay runs.
type testSignIn struct {
	mu      sync.Mutex
	account string
	refused []string
	retired bool
}

func (s *testSignIn) set(account string) {
	s.mu.Lock()
	s.account = account
	s.mu.Unlock()
}

func (s *testSignIn) Account() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.account
}

func (s *testSignIn) Token(_ context.Context, account string) (string, error) {
	if account == "" || account != s.Account() {
		return "", nil
	}
	return "token-" + account, nil
}

func (s *testSignIn) Refused(_ context.Context, token string) {
	s.mu.Lock()
	s.refused = append(s.refused, token)
	s.mu.Unlock()
}

func (s *testSignIn) Retired(string) {
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
}

// signInUpstream records each request's bearer and team, answering with the next status.
type signInUpstream struct {
	mu       sync.Mutex
	srv      *httptest.Server
	got      []string
	sent     [][]byte // each request's body
	statuses []int
	body     []byte
}

func newSignInUpstream(t *testing.T, statuses ...int) *signInUpstream {
	u := &signInUpstream{statuses: statuses}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.got = append(u.got, r.Header.Get("Authorization")+" "+r.Header.Get(ProjectHeader))
		u.sent = append(u.sent, sent)
		status := http.StatusOK
		if len(u.statuses) > 0 {
			status, u.statuses = u.statuses[0], u.statuses[1:]
		}
		body := u.body
		u.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *signInUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.got...)
}

// signedInRelay runs a relay that sends with signIn, its outbox dir, every session claimed
// for p1 under a policy that names no key. What an older relay queued, in
// legacyOutbox(dir), is taken in under the decision legacy, "" for none.
func signedInRelay(t *testing.T, u *signInUpstream, signIn SignIn, dir, legacy string) *Relay {
	t.Helper()
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, SignIn: signIn,
		LegacyDir: legacyOutbox(dir), LegacyOwner: decided(t, dir, legacy),
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func oneLog(session string) []byte {
	b, _ := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{
		LogRecords: []*logspb.LogRecord{{Attributes: []*commonpb.KeyValue{kv("session.id", session)}}}}}}}})
	return b
}

// decided is Options.LegacyOwner for the decision legacy, "" for none, with what the older
// outbox beside dir holds set aside as taking it does (auth.LegacyStage).
func decided(t *testing.T, dir, legacy string) func() string {
	t.Helper()
	if legacy != "" {
		if err := StageLegacyOutbox(legacyOutbox(dir)); err != nil {
			t.Fatal(err)
		}
	}
	return func() string { return legacy }
}

// legacyOutbox is where tests keep an older relay's outbox beside the outbox dir.
func legacyOutbox(dir string) string { return dir + "-legacy" }

// queue writes a part into dir's outbox under account, as an earlier relay left it.
func queue(t *testing.T, dir, account string) { t.Helper(); queueAt(t, dir, account, time.Now()) }

// queueAt is queue for a part that arrived at.
func queueAt(t *testing.T, dir, account string, at time.Time) {
	t.Helper()
	rt := routeOf(account, claim.Claim{ProjectID: "p1"})
	if err := (outbox{dir: dir}).put(rt, claim.Claim{}.Repository, newEntry(at, Logs, 1), oneLog("old")); err != nil {
		t.Fatal(err)
	}
}

// A part leaves with the sign-in's token, naming its team, and only under the account that
// collected it: another person's backlog waits for that person's sign-in.
func TestPartsLeaveOnlyUnderTheirOwnSignIn(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, accountB)
	signIn := &testSignIn{account: accountA}
	r := signedInRelay(t, u, signIn, dir, "")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("new"), "application/x-protobuf", token, false)
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q, want account A's token naming p1", got)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(u.requests()); n != 1 {
		t.Fatalf("%d requests: account B's backlog left under A", n)
	}
	// B signs in: B's backlog leaves, under B.
	signIn.set(accountB)
	r.sender(routeOf(accountB, claim.Claim{ProjectID: "p1"})).poke()
	waitFor(t, func() bool { return len(u.requests()) == 2 })
	if got := u.requests()[1]; got != "Bearer token-"+accountB+" p1" {
		t.Fatalf("sent %q, want account B's token", got)
	}
}

// What an older relay queued, in its own outbox, is taken into the account the retirement
// decided, and leaves under that account only: under another sign-in it waits in that
// account's lane. A route of the older outbox already in a lane moves as it is.
func TestABacklogFromBeforeUserTokensLeavesUnderItsAccount(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir), "")
	signedInRelay(t, u, &testSignIn{account: accountA}, dir, accountA)
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q, want A's token", got)
	}

	u2 := newSignInUpstream(t)
	dir2 := t.TempDir()
	queue(t, legacyOutbox(dir2), "")
	queue(t, legacyOutbox(dir2), accountA)
	signedInRelay(t, u2, &testSignIn{account: accountA}, dir2, accountB)
	waitFor(t, func() bool { return len(u2.requests()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := u2.requests(); len(got) != 1 || got[0] != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q: want only A's own part, B's backlog kept", got)
	}
	if entries, _ := (outbox{dir: dir2}).list(routeOf(accountB, claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("B's lane holds %d parts, want the older relay's one", len(entries))
	}
	if routes, _ := (outbox{dir: legacyOutbox(dir2)}).routes(); len(routes) != 0 {
		for _, rt := range routes {
			if entries, _ := (outbox{dir: legacyOutbox(dir2)}).list(rt); len(entries) != 0 {
				t.Fatalf("the older outbox kept %d parts on %s", len(entries), rt)
			}
		}
	}
}

// With no decision yet, an older relay's outbox is left as it is: nothing is taken in.
func TestNoDecisionTakesNothingFromAnOlderRelay(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, legacyOutbox(dir), "")
	r := signedInRelay(t, u, &testSignIn{account: accountA}, dir, "")
	r.janitor(time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d requests: an older relay's part left with no decision", n)
	}
	if entries, _ := (outbox{dir: legacyOutbox(dir)}).list(routeOf("", claim.Claim{ProjectID: "p1"})); len(entries) != 1 {
		t.Fatalf("the older outbox holds %d parts, want its one left", len(entries))
	}
	if routes, _ := (outbox{dir: dir}).routes(); len(routes) != 0 {
		t.Fatalf("routes %v were made with no decision", routes)
	}
}

// A 401 hands the token back to be rotated and the part is sent again; a 403 the sign-in's
// retirement reports it; neither drops the part.
func TestARefusedTokenIsRotatedAndThePartKept(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusUnauthorized)
	signIn := &testSignIn{account: accountA}
	r := signedInRelay(t, u, signIn, t.TempDir(), "")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("new"), "application/x-protobuf", token, false)
	// The relay counts the part once it reads the 200, after the upstream has seen it.
	waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 1 })
	if n := len(u.requests()); n != 2 {
		t.Fatalf("%d requests, want the refused one and its retry", n)
	}
	signIn.mu.Lock()
	refused := append([]string(nil), signIn.refused...)
	signIn.mu.Unlock()
	if len(refused) != 1 || refused[0] != "token-"+accountA {
		t.Fatalf("refused = %v", refused)
	}
}

// What the outbox drops for room or age is counted where doctor reads it.
func TestOutboxLossesAreRecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	queue(t, dir, accountA)
	old := time.Now().Add(-maxOutboxAge - time.Hour)
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	r := newRelay(Options{Dir: dir, Token: token})
	r.sweepOutbox(time.Now())
	l, err := ReadOutboxLosses(dir)
	if err != nil || l.Counts["outbox_expired"] != 1 || l.Counts["outbox_full"] != 0 || l.Last.IsZero() {
		t.Fatalf("losses = %+v, %v", l, err)
	}
	if routes, _ := r.outbox.routes(); len(routes) != 0 {
		t.Fatalf("routes left: %v", routes)
	}
}

// switchingSignIn is a sign-in that switches to another account as a token is asked for:
// the race between deciding a route may leave and sending it.
type switchingSignIn struct {
	testSignIn
	to string
}

func (s *switchingSignIn) Token(ctx context.Context, account string) (string, error) {
	s.set(s.to)
	return s.testSignIn.Token(ctx, account)
}

// A send asks for the token of the account that collected the parts: a sign-in switched
// at that moment gives none, so A's parts never leave with B's token.
func TestASwitchMidSendNeverLendsTheNewSignIn(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, accountA)
	signedInRelay(t, u, &switchingSignIn{testSignIn: testSignIn{account: accountA}, to: accountB}, dir, "")
	time.Sleep(200 * time.Millisecond)
	if got := u.requests(); len(got) != 0 {
		t.Fatalf("sent %q: A's parts left after B signed in", got)
	}
}

// A part is bound to the sign-in in place when it arrives, however long it waits for its
// team's policy: once B has signed in, A's held part never leaves as B's, nor names B,
// whom the policy it is placed under names.
func TestAHeldPartKeepsTheAccountItArrivedUnder(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &testSignIn{account: accountA}
	var mu sync.Mutex
	policyReady := false
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			mu.Lock()
			defer mu.Unlock()
			if !policyReady {
				return Policy{}, ErrNoKey
			}
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true, User: "user-b", UserAccount: accountB}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("held"), "application/x-protobuf", token, false)
	signIn.set(accountB)
	mu.Lock()
	policyReady = true
	mu.Unlock()
	r.cache.expire(time.Now().Add(time.Hour))
	r.sweep()
	time.Sleep(200 * time.Millisecond)
	if got := u.requests(); len(got) != 0 {
		t.Fatalf("sent %q: A's held part left under B", got)
	}
	signIn.set(accountA)
	r.sender(routeOf(accountA, claim.Claim{ProjectID: "p1"})).poke()
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q, want A's token", got)
	}
	var sent logspb.LogsData
	u.mu.Lock()
	err := proto.Unmarshal(u.sent[0], &sent)
	u.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := attr(sent.ResourceLogs[0].GetResource().GetAttributes(), semconv.TermaUserIDKey); got != "" {
		t.Fatalf("A's part left naming %q", got)
	}
}

// A profile set up with a server key sends only what no person's sign-in collected, its
// no_person lane: a person's bound backlog, an older relay's the retirement decided was a
// person's, and a route bound to no lane are never sent with the key.
func TestAServerKeyNeverSendsAPersonsBacklog(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, accountA)
	queue(t, legacyOutbox(dir), "")    // an older relay's: the retired sign-ins' person's
	queue(t, dir, sendas.NoPersonLane) // the key's own
	queue(t, dir, "")                  // no lane's, which this version never writes
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{account: sendas.NoPersonLane},
		LegacyDir: legacyOutbox(dir), LegacyOwner: decided(t, dir, accountB),
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "server-key", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if got := u.requests(); len(got) != 1 || got[0] != "Bearer server-key " {
		t.Fatalf("sent %q, want only the part no person collected, with the key", got)
	}
}

// Losses are counted from the first one after a quiet window: older counts start over.
func TestOutboxLossesStartOverAfterAQuietWindow(t *testing.T) {
	t.Parallel()
	r := newRelay(Options{Dir: t.TempDir(), Token: token})
	start := time.Now()
	r.recordLosses(map[string]int{"outbox_full": 3}, start)
	r.recordLosses(map[string]int{"outbox_expired": 1}, start.Add(time.Hour))
	if l, _ := ReadOutboxLosses(r.outbox.dir); l.Counts["outbox_full"] != 3 || l.Counts["outbox_expired"] != 1 {
		t.Fatalf("losses = %+v, want both counted", l)
	}
	r.recordLosses(map[string]int{"outbox_expired": 2}, start.Add(lossWindow+2*time.Hour))
	if l, _ := ReadOutboxLosses(r.outbox.dir); l.Counts["outbox_full"] != 0 || l.Counts["outbox_expired"] != 2 {
		t.Fatalf("losses = %+v, want only the new loss", l)
	}
}

// switchOnRead is an export body whose first read switches the sign-in, as a switch landing
// while the relay reads the request would.
type switchOnRead struct {
	r      io.Reader
	onRead func()
	once   sync.Once
}

func (b *switchOnRead) Read(p []byte) (int, error) {
	b.once.Do(b.onRead)
	return b.r.Read(p)
}

// An export is bound to the sign-in it arrived under, even when another one takes its place
// while its body is still being read: it leaves under A's token, not B's.
func TestAnExportKeepsTheSignInItArrivedUnder(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &testSignIn{account: accountA}
	r := signedInRelay(t, u, signIn, t.TempDir(), "")
	body := &switchOnRead{r: bytes.NewReader(oneLog("new")), onRead: func() { signIn.set(accountB) }}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", body)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export answered %d: %s", w.Code, w.Body)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(u.requests()); n != 0 {
		t.Fatalf("%d requests left under B: %v", n, u.requests())
	}
	// A signs back in: the part, bound to A, leaves under A.
	signIn.set(accountA)
	r.sender(routeOf(accountA, claim.Claim{ProjectID: "p1"})).poke()
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q, want account A's token", got)
	}
}

// serverKeyRelay runs a relay on dir under a server key, what an older relay queued in
// legacyOutbox(dir) taken in under the decision legacy.
func serverKeyRelay(t *testing.T, u *signInUpstream, dir, legacy string) *Relay {
	t.Helper()
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{account: sendas.NoPersonLane},
		LegacyDir: legacyOutbox(dir), LegacyOwner: decided(t, dir, legacy),
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "server-key", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// A merged batch refused is sent again a part at a time.
func TestARefusedBatchIsSentAPartAtATime(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusBadRequest)
	dir := t.TempDir()
	queueAt(t, dir, sendas.NoPersonLane, time.Now().Add(time.Minute))
	queueAt(t, dir, sendas.NoPersonLane, time.Now().Add(2*time.Minute))
	r := serverKeyRelay(t, u, dir, "")
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 2 })
	if n := len(u.requests()); n != 3 {
		t.Fatalf("%d requests; want the refused batch, then each part alone", n)
	}
}

// A sign-in refused as retired is marked retired whatever status the refusal comes with,
// a 401 included: never handed back to be refreshed as an expired token.
func TestARetiredRefusalIsNeverTakenForAnExpiredToken(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusUnauthorized)
	u.body = []byte(`{"code":16,"message":"retired","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`)
	signIn := &testSignIn{account: accountA}
	dir := t.TempDir()
	queue(t, dir, accountA)
	signedInRelay(t, u, signIn, dir, "")
	waitFor(t, func() bool {
		signIn.mu.Lock()
		defer signIn.mu.Unlock()
		return signIn.retired
	})
	signIn.mu.Lock()
	defer signIn.mu.Unlock()
	if len(signIn.refused) != 0 {
		t.Fatalf("a retired sign-in's token was handed back as expired: %v", signIn.refused)
	}
}

// tokenlessSignIn is a sign-in in place that gives no token, as a retired one does.
type tokenlessSignIn struct{ testSignIn }

func (s *tokenlessSignIn) Token(context.Context, string) (string, error) { return "", nil }

// A route whose sign-in gives no token (retired, say) waits as one no sign-in may send: the
// relay reads as idle, and nothing counts as an upstream retry.
func TestARouteWithNoTokenWaitsIdle(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, accountA)
	r := signedInRelay(t, u, &tokenlessSignIn{testSignIn{account: accountA}}, dir, "")
	waitForCounters(t, r, func(c map[string]int) bool { return c["recovered_from_outbox"] == 1 })
	time.Sleep(200 * time.Millisecond) // a pass, which would count a retry
	if _, idle := r.Idle(); !idle {
		t.Fatal("the relay reads as busy while no sign-in may send its parts")
	}
	if c := r.Stats().Snapshot().Counters; c["upstream_retries"] != 0 || len(u.requests()) != 0 {
		t.Fatalf("counters %v, %d sent; want it waiting, not retried", c, len(u.requests()))
	}
}
