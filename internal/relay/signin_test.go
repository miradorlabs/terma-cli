package relay

import (
	"bytes"
	"context"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
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
	statuses []int
	body     []byte
}

func newSignInUpstream(t *testing.T, statuses ...int) *signInUpstream {
	u := &signInUpstream{statuses: statuses}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.got = append(u.got, r.Header.Get("Authorization")+" "+r.Header.Get(ProjectHeader))
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
// for p1 under a policy that names no key.
func signedInRelay(t *testing.T, u *signInUpstream, signIn SignIn, dir, legacy string) *Relay {
	t.Helper()
	legacyFn := func() (string, time.Time) { return legacy, time.Now().Add(time.Hour) }
	r := newRelay(Options{Dir: dir, Token: token, Hold: time.Minute, SignIn: signIn, Legacy: legacyFn,
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

// What a relay queued before user tokens leaves under the account its retired sign-ins
// belonged to, and under no other.
func TestABacklogFromBeforeUserTokensLeavesUnderItsAccount(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, "")
	signedInRelay(t, u, &testSignIn{account: accountA}, dir, accountA)
	waitFor(t, func() bool { return len(u.requests()) == 1 })

	u2 := newSignInUpstream(t)
	dir2 := t.TempDir()
	queue(t, dir2, "")
	signedInRelay(t, u2, &testSignIn{account: accountA}, dir2, accountB)
	time.Sleep(100 * time.Millisecond)
	if n := len(u2.requests()); n != 0 {
		t.Fatalf("%d requests: a backlog left under another account", n)
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
	if err != nil || l.Expired != 1 || l.Full != 0 || l.Last.IsZero() {
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
// team's policy: once B has signed in, A's held part never leaves as B's.
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
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true}, nil
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
}

// A part bound to no account leaves under the retired sign-ins' account only if it arrived
// before they were retired; one that arrived after, with no sign-in, leaves under none.
func TestOnlyABacklogFromBeforeRetirementCountsAsTheOlderSignIns(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	queue(t, dir, "")
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{account: accountA},
		Legacy: func() (string, time.Time) { return accountA, time.Now().Add(-time.Hour) },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(200 * time.Millisecond)
	if got := u.requests(); len(got) != 0 {
		t.Fatalf("sent %q: a part from after the retirement left as the older sign-ins'", got)
	}
}

// A profile set up with a server key sends only what no person's sign-in collected: a
// person's bound backlog, or an older terma's that counts as its retired sign-ins', is
// never sent with the key.
func TestAServerKeyNeverSendsAPersonsBacklog(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	retired := time.Now()
	queueAt(t, dir, accountA, retired.Add(-time.Minute))
	queueAt(t, dir, "", retired.Add(-time.Minute)) // an older terma's: the retired sign-ins'
	queueAt(t, dir, "", retired.Add(time.Minute))  // after the retirement: no person's
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{},
		Legacy: func() (string, time.Time) { return accountB, retired },
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
	o := outbox{dir: t.TempDir()}
	start := time.Now()
	o.recordLosses(3, 0, start)
	o.recordLosses(0, 1, start.Add(time.Hour))
	if l, _ := ReadOutboxLosses(o.dir); l.Full != 3 || l.Expired != 1 {
		t.Fatalf("losses = %+v, want both counted", l)
	}
	o.recordLosses(0, 2, start.Add(lossWindow+2*time.Hour))
	if l, _ := ReadOutboxLosses(o.dir); l.Full != 0 || l.Expired != 2 {
		t.Fatalf("losses = %+v, want only the new loss", l)
	}
}

// Under a server key, an older terma's part at the head of one signal holds up no other: the
// sender takes the oldest signal with a part the key may send.
func TestABlockedSignalHoldsUpNoOtherUnderAServerKey(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	retired := time.Now()
	rt := routeOf("", claim.Claim{ProjectID: "p1"})
	span, _ := proto.Marshal(&tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{
		Spans: []*tracepb.Span{{Name: "old", Attributes: []*commonpb.KeyValue{kv("session.id", "old")}}}}}}}})
	// The older terma's trace is the oldest part on the route; the key's log came after.
	if err := (outbox{dir: dir}).put(rt, claim.Claim{}.Repository, newEntry(retired.Add(-time.Hour), Traces, 1), span); err != nil {
		t.Fatal(err)
	}
	queueAt(t, dir, "", retired.Add(time.Minute))
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{},
		Legacy: func() (string, time.Time) { return "ambiguous", retired },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "server-key", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
	if c := r.Stats().Snapshot().Counters; c["forwarded.traces"] != 0 {
		t.Fatalf("the older terma's trace left under the key: %v", c)
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

// A part that arrived bound to no account before the older sign-ins were retired is theirs,
// however long it waited in the hold: released after the retirement, it still leaves under
// their account.
func TestAHeldPartKeepsTheTimeItArrived(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	signIn := &testSignIn{} // signed out
	var mu sync.Mutex
	policyReady := false
	var retiredAt time.Time // no record yet
	r := newRelay(Options{Dir: t.TempDir(), Token: token, Hold: time.Minute, SignIn: signIn,
		Legacy: func() (string, time.Time) {
			mu.Lock()
			defer mu.Unlock()
			return accountA, retiredAt
		},
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			mu.Lock()
			defer mu.Unlock()
			if !policyReady {
				return Policy{}, ErrNoKey
			}
			return Policy{Endpoint: u.srv.URL, IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("held"), "application/x-protobuf", token, false)
	time.Sleep(10 * time.Millisecond)
	// A signs in, which retires the older sign-ins, A's own; then the policy arrives.
	mu.Lock()
	retiredAt, policyReady = time.Now(), true
	mu.Unlock()
	signIn.set(accountA)
	time.Sleep(10 * time.Millisecond)
	r.cache.expire(time.Now().Add(time.Hour))
	r.sweep()
	waitFor(t, func() bool { return len(u.requests()) == 1 })
	if got := u.requests()[0]; got != "Bearer token-"+accountA+" p1" {
		t.Fatalf("sent %q, want A's token", got)
	}
}

// serverKeyRelay runs a relay on dir under a server key, the older sign-ins named no one
// and retired at retired.
func serverKeyRelay(t *testing.T, u *signInUpstream, dir string, retired time.Time) *Relay {
	t.Helper()
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{},
		Legacy: func() (string, time.Time) { return "ambiguous", retired },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "server-key", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// An older terma's backlog longer than a batch, at the head of a signal, holds up none of
// the key's own parts behind it.
func TestALongBacklogHoldsUpNoKeysPartBehindIt(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	retired := time.Now()
	for i := range maxMergeFiles + 1 {
		queueAt(t, dir, "", retired.Add(-time.Hour+time.Duration(i)*time.Millisecond))
	}
	queueAt(t, dir, "", retired.Add(time.Minute))
	r := serverKeyRelay(t, u, dir, retired)
	waitForCounters(t, r, func(c map[string]int) bool { return c["forwarded.logs"] == 1 })
}

// A merged batch refused behind an older terma's part is sent again a part at a time: the
// part blocked at the head does not stop the key's own parts going one by one.
func TestARefusedBatchIsSentAPartAtATimePastABlockedHead(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t, http.StatusBadRequest)
	dir := t.TempDir()
	retired := time.Now()
	queueAt(t, dir, "", retired.Add(-time.Hour))
	queueAt(t, dir, "", retired.Add(time.Minute))
	queueAt(t, dir, "", retired.Add(2*time.Minute))
	r := serverKeyRelay(t, u, dir, retired)
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

// A part collected while the sign-in could not be read is kept, on its own route, and never
// leaves under a server key: it may be a person's.
func TestAnUnreadableSignInsPartIsKeptButNeverSentByAKey(t *testing.T) {
	t.Parallel()
	u := newSignInUpstream(t)
	dir := t.TempDir()
	r := newRelay(Options{Dir: dir, Token: token, SignIn: &testSignIn{account: auth.AccountUnreadable},
		Lookup: func(string, time.Time) (claim.Claim, bool) { return claim.Claim{ProjectID: "p1"}, true },
		Resolve: func(claim.Claim) (Policy, error) {
			return Policy{Endpoint: u.srv.URL, Key: "server-key", IncludePrompts: true, IncludeToolContent: true}, nil
		}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	post(t, srv, "/v1/logs", oneLog("unreadable"), "application/x-protobuf", token, false)
	time.Sleep(100 * time.Millisecond)
	if c := r.Stats().Snapshot().Counters; c["dropped.outbox_write_failed.logs"] != 0 || len(u.requests()) != 0 {
		t.Fatalf("counters %v, %d sent; want it kept and not sent", c, len(u.requests()))
	}
	if entries, err := r.outbox.list(routeOf(auth.AccountUnreadable, claim.Claim{ProjectID: "p1"})); err != nil || len(entries) != 1 {
		t.Fatalf("queued %v (%v); want the part on its own route", entries, err)
	}
}
