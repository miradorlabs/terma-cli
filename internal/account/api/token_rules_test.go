package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// shortRetries shortens the refresh's retry timings for one test. Tests that call it do not
// run in parallel, since the timings are the package's.
func shortRetries(t *testing.T, budget, wait time.Duration) {
	t.Helper()
	oldBudget, oldWait := refreshAttemptBudget, defaultRetryAfter
	refreshAttemptBudget, defaultRetryAfter = budget, wait
	t.Cleanup(func() { refreshAttemptBudget, defaultRetryAfter = oldBudget, oldWait })
}

// refreshScript answers each refresh with the next of its answers; "ok" rotates the pair.
type refreshScript struct {
	mu        sync.Mutex
	answers   []string
	presented []string
	bearer    []string
	onAnswer  func(n int)
}

func (s *refreshScript) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != tokenPath {
		w.Write([]byte(`{}`))
		return
	}
	var body tokenRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	n := len(s.presented)
	s.presented = append(s.presented, body.RefreshToken)
	s.bearer = append(s.bearer, r.Header.Get("Authorization"))
	answer := "ok"
	if n < len(s.answers) {
		answer = s.answers[n]
	}
	hook := s.onAnswer
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	switch answer {
	case "409":
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"code":"ABORTED","reason":"REFRESH_SUPERSEDED","message":"superseded"}}`))
	case "503":
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"code":"UNAVAILABLE","message":"try again"}}`))
	case "429":
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"TOO_MANY_REQUESTS","message":"slow down"}}`))
	case "401":
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid refresh token"}}`))
	case "401 bare":
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("Unauthorized\n"))
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("access-%d", n+1), "refresh_token": fmt.Sprintf("refresh-%d", n+1),
			"token_type": "Bearer", "expires_in": 3600, "session_id": "session-test", "family_id": "session-test",
			"user": map[string]string{"id": "user-1"},
		})
	}
}

func (s *refreshScript) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.presented...)
}

// expiredClient is a client whose stored pair needs a refresh, against script.
func expiredClient(t *testing.T, script *refreshScript) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(script.serve))
	t.Cleanup(srv.Close)
	return newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour), UserID: "user-1"}, "")
}

func storedPair(t *testing.T, c *Client) *auth.Credential {
	t.Helper()
	cred, err := auth.LoadCredential(c.dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// A refresh sends the refresh token as its only credential, and stores the new pair.
func TestRefreshSendsNoBearer(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if script.bearer[0] != "" {
		t.Fatal("the refresh sent the access token too")
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-1" || got.FamilyID != "session-test" {
		t.Fatalf("stored %+v, want the rotated pair", got)
	}
}

// A 409 REFRESH_SUPERSEDED after which the store holds another process's rotation takes
// that pair and presents nothing more.
func TestSupersededRefreshTakesTheStoredPair(t *testing.T) {
	script := &refreshScript{answers: []string{"409"}}
	c := expiredClient(t, script)
	script.onAnswer = func(int) {
		// A winner that does not take this machine's lock, such as another copy of the store,
		// has stored its pair by the time the loser hears 409. Written past the held lock.
		won := map[string]*auth.Credential{config.DefaultProfile: {AccessToken: "access-won", RefreshToken: "refresh-won",
			ExpiresAt: time.Now().Add(time.Hour), SessionID: "session-test", AuthURL: c.authURL}}
		data, _ := json.Marshal(won)
		_ = os.WriteFile(config.CredentialsPath(c.dir), data, 0o600)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := script.sent(); len(sent) != 1 {
		t.Fatalf("presented %v, want one refresh", sent)
	}
	if c.Credential().AccessToken != "access-won" {
		t.Fatalf("held %q, want the stored winner's pair", c.Credential().AccessToken)
	}
}

// A 409 with no newer pair stored retries once, at once, with the same token.
func TestSupersededRefreshRetriesOnce(t *testing.T) {
	script := &refreshScript{answers: []string{"409", "ok"}}
	c := expiredClient(t, script)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := script.sent(); len(sent) != 2 || sent[0] != "refresh-0" || sent[1] != "refresh-0" {
		t.Fatalf("presented %v, want refresh-0 twice", sent)
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-2" {
		t.Fatalf("stored %q, want the retry's pair", got.RefreshToken)
	}
}

// A second 409 ends the sign-in: the spent token is never presented again.
func TestASecondSupersededRefreshEndsTheSignIn(t *testing.T) {
	script := &refreshScript{answers: []string{"409", "409"}}
	c := expiredClient(t, script)
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want signed out", err)
	}
	if sent := script.sent(); len(sent) != 2 {
		t.Fatalf("presented %v, want exactly two", sent)
	}
	if _, err := auth.LoadCredential(c.dir, config.DefaultProfile); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("the stored sign-in survived: %v", err)
	}
}

// A 503 rotated nothing it says: the same token is retried after Retry-After, and the
// stored pair records when the refresh came into doubt until one succeeds.
func TestAnUnavailableRefreshIsRetried(t *testing.T) {
	shortRetries(t, 10*time.Second, 10*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "503", "ok"}}
	c := expiredClient(t, script)
	var marked time.Time
	script.onAnswer = func(n int) {
		if n == 1 {
			cred, _ := auth.LoadCredential(c.dir, config.DefaultProfile)
			marked = cred.RefreshInDoubtSince
		}
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := script.sent(); len(sent) != 3 || sent[2] != "refresh-0" {
		t.Fatalf("presented %v, want refresh-0 three times", sent)
	}
	if marked.IsZero() {
		t.Fatal("the stored pair did not record the refresh in doubt")
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-3" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the new pair with no refresh in doubt", got)
	}
}

// Past the budget the sign-in ends: retrying later could present a spent token after the
// server's grace, which revokes the whole family.
func TestAnUnavailableRefreshStopsAtTheBudget(t *testing.T) {
	shortRetries(t, 300*time.Millisecond, 100*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "503", "503", "503", "503", "503", "503", "503"}}
	c := expiredClient(t, script)
	start := time.Now()
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want the sign-in ended", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the retries outlived the budget")
	}
	if _, err := auth.LoadCredential(c.dir, config.DefaultProfile); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("the stored sign-in survived: %v", err)
	}
}

// A refresh another process left in doubt past the budget is never presented again.
func TestARefreshInDoubtPastTheBudgetIsNeverPresented(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	cred := storedPair(t, c)
	cred.RefreshInDoubtSince = time.Now().Add(-2 * refreshAttemptBudget)
	if err := auth.UpdateCredential(c.dir, config.DefaultProfile, cred); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want the sign-in ended", err)
	}
	if sent := script.sent(); len(sent) != 0 {
		t.Fatalf("presented %v", sent)
	}
}

// A request that never reached the server rotated nothing: the refresh is not in doubt.
func TestAnUnreachableAuthHostLeavesNothingInDoubt(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := newTestClient(t, url, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour)}, "")
	if err := c.Refresh(context.Background()); err == nil || errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want a network error that keeps the sign-in", err)
	}
	if got := storedPair(t, c); !got.RefreshInDoubtSince.IsZero() || got.RefreshToken != "refresh-0" {
		t.Fatalf("stored %+v, want the pair untouched", got)
	}
}

// A revoke sends the refresh token in its body and nothing else, and reads the answer.
func TestRevokeRefreshToken(t *testing.T) {
	t.Parallel()
	var body map[string]string
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != revokePath {
			http.NotFound(w, r)
			return
		}
		bearer = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"enforcement":"PENDING"}`))
	}))
	defer srv.Close()
	got, err := RevokeRefreshToken(context.Background(), srv.URL, "test", "refresh-1")
	if err != nil || got != Pending {
		t.Fatalf("RevokeRefreshToken = %q, %v", got, err)
	}
	if bearer != "" || len(body) != 1 || body["refresh_token"] != "refresh-1" {
		t.Fatalf("sent %v with bearer %q", body, bearer)
	}
}

// The gateway's reasons are read, and only ORG_NOT_ACCESSIBLE says access is lost.
func TestErrorReasons(t *testing.T) {
	t.Parallel()
	unavailable := &Error{StatusCode: http.StatusServiceUnavailable, Reason: ReasonAuthorizationUnavailable}
	gone := &Error{StatusCode: http.StatusForbidden, Reason: ReasonOrgNotAccessible}
	denied := &Error{StatusCode: http.StatusForbidden}
	if unavailable.OrgNotAccessible() {
		t.Error("AUTHORIZATION_UNAVAILABLE says nothing about access")
	}
	if !gone.OrgNotAccessible() {
		t.Error("ORG_NOT_ACCESSIBLE is the loss of access")
	}
	if denied.OrgNotAccessible() {
		t.Error("a 403 without the reason is not the loss of access")
	}
}

// A refresh whose answer is lost (the connection cut after the request went out) may have
// rotated: it is retried with the same token, within the grace, as a 503 is.
func TestALostRefreshAnswerIsRetried(t *testing.T) {
	shortRetries(t, 10*time.Second, 10*time.Millisecond)
	var cuts atomic.Int32
	script := &refreshScript{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tokenPath && cuts.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		script.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour)}, "")
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := script.sent(); len(sent) != 1 || sent[0] != "refresh-0" || cuts.Load() != 2 {
		t.Fatalf("presented %v after %d attempts, want refresh-0 again after the cut", sent, cuts.Load())
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-1" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the rotation with no doubt left", got)
	}
}

// A caller that has gone sends nothing, and leaves nothing in doubt.
func TestAnEndedCallerSendsNothing(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.refresh(ctx, func(_, current *auth.Credential) bool { return true }); err == nil {
		t.Fatal("an ended caller's refresh succeeded")
	}
	if sent := script.sent(); len(sent) != 0 {
		t.Fatalf("presented %v", sent)
	}
	if got := storedPair(t, c); !got.RefreshInDoubtSince.IsZero() {
		t.Fatal("an unsent refresh left a mark")
	}
}

// A caller that goes once its token has left does not lose the rotation: the exchange
// outlives it, retries a lost answer within the budget, and saves the new pair.
func TestARotationOutlivesItsCaller(t *testing.T) {
	shortRetries(t, 10*time.Second, 10*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "ok"}}
	c := expiredClient(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	script.onAnswer = func(n int) {
		if n == 0 {
			cancel()
		}
	}
	if err := c.refresh(ctx, func(_, current *auth.Credential) bool { return true }); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if sent := script.sent(); len(sent) != 2 || sent[1] != "refresh-0" {
		t.Fatalf("presented %v, want refresh-0 again after the caller went", sent)
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-2" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the rotation with no doubt left", got)
	}
}

// The auth gateway's own 401 ends the sign-in: the stored pair goes, so no process
// presents the refused token again.
func TestARefusedRefreshRemovesTheSignIn(t *testing.T) {
	script := &refreshScript{answers: []string{"401"}}
	c := expiredClient(t, script)
	err := c.Refresh(context.Background())
	if !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("refresh = %v, want the sign-in over", err)
	}
	if _, err := auth.LoadCredential(c.dir, config.DefaultProfile); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("the refused sign-in is still stored (%v)", err)
	}
}

// A 401 without the gateway's envelope, a proxy's, signs no one out and leaves nothing in
// doubt: the request never reached the auth host.
func TestABare401KeepsTheSignIn(t *testing.T) {
	script := &refreshScript{answers: []string{"401 bare"}}
	c := expiredClient(t, script)
	err := c.Refresh(context.Background())
	if err == nil || errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("refresh = %v, want a plain error", err)
	}
	// `terma auth token` reads an *Error's 401 as signed out.
	if _, ok := errors.AsType[*Error](err); ok {
		t.Fatalf("a bare 401 reads as the auth host's answer: %v", err)
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-0" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the pair kept with no doubt", got)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("the next refresh: %v", err)
	}
}

// A refresh that fails before any connection is made, as on a TLS failure (a captive
// portal's certificate), sent nothing: the sign-in stays, and nothing is left in doubt.
func TestATLSFailureLeavesNothingInDoubt(t *testing.T) {
	shortRetries(t, 0, time.Millisecond)
	script := &refreshScript{}
	srv := httptest.NewTLSServer(http.HandlerFunc(script.serve))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour)}, "")
	err := c.Refresh(context.Background())
	if err == nil || errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("refresh = %v, want a plain error", err)
	}
	if sent := script.sent(); len(sent) != 0 {
		t.Fatalf("presented %v through a failed handshake", sent)
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-0" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the pair kept with no doubt", got)
	}
}

// A refresh in doubt keeps trying within the budget when a retry fails before it connects:
// a later answer may still return the rotation.
func TestARetryThatCannotConnectKeepsTrying(t *testing.T) {
	shortRetries(t, 10*time.Second, 10*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "ok"}}
	c := expiredClient(t, script)
	var calls atomic.Int32
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == tokenPath && calls.Add(1) == 2 {
			return nil, errors.New("tls: failed to verify certificate")
		}
		return base.RoundTrip(r)
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d attempts, want 3", calls.Load())
	}
	if got := storedPair(t, c); got.RefreshToken != "refresh-2" || !got.RefreshInDoubtSince.IsZero() {
		t.Fatalf("stored %+v, want the rotation with no doubt left", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A body the guard holds back never carries the token: the request fails as past the
// budget, and the server reads no refresh token.
func TestATokenHeldBackPastTheBudgetIsNotSent(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	_, err := c.postToken(context.Background(), func() bool { return false },
		tokenRequest{GrantType: "refresh_token", RefreshToken: "refresh-0"})
	if !errors.Is(err, errPastBudget) {
		t.Fatalf("postToken = %v, want held back", err)
	}
	for _, sent := range script.sent() {
		if sent != "" {
			t.Fatalf("the server read %q", sent)
		}
	}
	if kind, _ := classifyRefresh(err); kind != refreshUnspent {
		t.Fatalf("a held-back refresh classified %d, want unspent", kind)
	}
}

// A mark recorded in the future (the clock went back since) is past the budget: how long
// ago the token left is unknown, so it is never presented again.
func TestAMarkInTheFutureIsPastTheBudget(t *testing.T) {
	script := &refreshScript{}
	srv := httptest.NewServer(http.HandlerFunc(script.serve))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour), RefreshInDoubtSince: time.Now().Add(10 * time.Minute).UTC()}, "")
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("refresh = %v, want the sign-in over", err)
	}
	if sent := script.sent(); len(sent) != 0 {
		t.Fatalf("presented %v under a mark from the future", sent)
	}
}

// A refused token this client could not remove from the store is still never presented
// again by it.
func TestARefusedTokenIsNotPresentedAgain(t *testing.T) {
	script := &refreshScript{answers: []string{"401", "ok"}}
	c := expiredClient(t, script)
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("refresh = %v, want the sign-in over", err)
	}
	// As if the removal had failed: the store holds the refused pair again.
	if _, err := auth.SaveCredential(c.dir, config.DefaultProfile, &auth.Credential{AccessToken: "access-0",
		RefreshToken: "refresh-0", ExpiresAt: time.Now().Add(-time.Hour), SessionID: "session-test", AuthURL: c.authURL}); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("a second refresh = %v, want the sign-in over", err)
	}
	if sent := script.sent(); len(sent) != 1 {
		t.Fatalf("presented %v, want the refused token once", sent)
	}
	if _, err := auth.LoadCredential(c.dir, config.DefaultProfile); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("the refused pair is still stored (%v)", err)
	}
}

// When the store missed a rotation and still can't take the live pair, nothing is sent:
// the refresh could not be recorded. The store's spent token is marked past every budget,
// so another process ends the sign-in rather than present it.
func TestASpentStoredTokenIsNeverPresented(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	// The store takes the mark, but not the pairs: a write that fails alone, as a
	// transient failure would.
	update := updateCredential
	updateCredential = func(*auth.Tx, string, *auth.Credential) error { return errors.New("disk full") }
	t.Cleanup(func() { updateCredential = update })
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("the rotation was stored by a store that fails")
	}
	if got := c.Credential().RefreshToken; got != "refresh-1" {
		t.Fatalf("the client holds %q, want the live refresh-1", got)
	}
	always := func(_, _ *auth.Credential) bool { return true }
	if err := c.refresh(context.Background(), always); err == nil {
		t.Fatal("a refresh went out with the store unable to keep the pair")
	}
	if sent := script.sent(); len(sent) != 1 {
		t.Fatalf("presented %v, want only the first refresh", sent)
	}
	stored := storedPair(t, c)
	if stored.RefreshToken != "refresh-0" || stored.RefreshInDoubtSince.IsZero() || withinBudget(stored.RefreshInDoubtSince, 0) {
		t.Fatalf("stored %+v, want the spent token marked past the budget", stored)
	}
	other, err := New(&config.Config{Dir: c.dir, ProfileName: config.DefaultProfile, APIURL: c.authURL, AuthURL: c.authURL}, Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.refresh(context.Background(), always); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("another process's refresh = %v, want the sign-in over", err)
	}
	if sent := script.sent(); len(sent) != 1 {
		t.Fatalf("another process presented %v", sent)
	}
}

// The mark is stored before the token leaves, so a process killed mid-request leaves it.
func TestTheRefreshIsMarkedBeforeItIsSent(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	var seen time.Time
	script.onAnswer = func(int) {
		data, _ := os.ReadFile(config.CredentialsPath(c.dir))
		var file map[string]*auth.Credential
		_ = json.Unmarshal(data, &file)
		if cred := file[config.DefaultProfile]; cred != nil {
			seen = cred.RefreshInDoubtSince
		}
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen.IsZero() {
		t.Fatal("the token left before the store said a refresh was under way")
	}
}

// After an attempt in doubt, an answer that rotated nothing (a 429, a bare 401) is retried
// within the budget, keeping the doubt meanwhile: a later answer may still return the
// rotation, rather than the next process ending a healthy sign-in.
func TestARefreshInDoubtRetriesPastAnAnswerThatRotatedNothing(t *testing.T) {
	for _, answer := range []string{"429", "401 bare"} {
		t.Run(answer, func(t *testing.T) {
			shortRetries(t, 10*time.Second, 10*time.Millisecond)
			script := &refreshScript{answers: []string{"503", answer, "ok"}}
			c := expiredClient(t, script)
			var doubt time.Time
			script.onAnswer = func(n int) {
				if n == 2 {
					cred, _ := auth.LoadCredential(c.dir, config.DefaultProfile)
					doubt = cred.RefreshInDoubtSince
				}
			}
			if err := c.Refresh(context.Background()); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if doubt.IsZero() {
				t.Fatalf("the %s cleared the doubt the 503 left", answer)
			}
			if got := storedPair(t, c); got.RefreshToken != "refresh-3" || !got.RefreshInDoubtSince.IsZero() {
				t.Fatalf("stored %+v, want the rotation with no doubt left", got)
			}
		})
	}
}

// A Retry-After longer than the budget left is cut short, so one last retry starts a
// margin before the budget ends, inside the server's grace, and a brief outage survives.
func TestALongRetryAfterIsCutToTheBudget(t *testing.T) {
	shortRetries(t, 500*time.Millisecond, 10*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "ok"}} // Retry-After: 1 s, past the budget
	c := expiredClient(t, script)
	start := time.Now()
	var second time.Duration
	script.onAnswer = func(n int) {
		if n == 1 {
			second = time.Since(start)
		}
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if sent := script.sent(); len(sent) != 2 || sent[1] != "refresh-0" {
		t.Fatalf("presented %v, want refresh-0 again", sent)
	}
	if second > 500*time.Millisecond {
		t.Fatalf("the retry started %v after the first, past the budget", second)
	}
}

// A mark names its session: one for a session no longer stored is refused.
func TestAMarkForAnotherSessionIsRefused(t *testing.T) {
	script := &refreshScript{}
	c := expiredClient(t, script)
	err := auth.Locked(context.Background(), c.dir, func(tx *auth.Tx) error {
		return tx.MarkRefreshInDoubt(config.DefaultProfile, "another-session", "refresh-0", time.Now())
	})
	if err == nil {
		t.Fatal("a mark for another session was taken")
	}
	data, _ := os.ReadFile(config.CredentialsPath(c.dir))
	if strings.Contains(string(data), "refresh_in_doubt_since") {
		t.Fatal("another session's mark reached the file")
	}
}

// A refresh refused as SIGN_IN_RETIRED marks the stored sign-in, for doctor, and reads as
// both signed out and retired.
func TestARetiredSignInIsMarked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","reason":"SIGN_IN_RETIRED","message":"retired"}}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour)}, "")
	err := c.Refresh(context.Background())
	if !errors.Is(err, auth.ErrSignInRetired) || !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want retired and signed out", err)
	}
	if !auth.Retired(c.dir, config.DefaultProfile) {
		t.Fatal("the stored sign-in was not marked retired")
	}
	if c.Credential().RetiredAt.IsZero() {
		t.Fatal("the client still holds the retired pair as live: its access token would be presented again")
	}
	if got := storedPair(t, c); !got.RefreshInDoubtSince.IsZero() {
		t.Fatal("a retired answer, which rotated nothing, left the refresh in doubt")
	}
	// Past the budget, the retired sign-in is still stored, and nothing is sent again.
	shortRetries(t, 0, time.Millisecond)
	if err := c.Refresh(context.Background()); !errors.Is(err, auth.ErrSignInRetired) {
		t.Fatalf("a second refresh = %v, want retired", err)
	}
	if !auth.Retired(c.dir, config.DefaultProfile) {
		t.Fatal("the retired sign-in was deleted")
	}
}

// Any other request a gateway refuses as SIGN_IN_RETIRED marks it too, and the client
// sends nothing more with it: the retired sign-in reads as signed out.
func TestARetiredAnswerElsewhereMarksTheSignIn(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":"PERMISSION_DENIED","reason":"SIGN_IN_RETIRED","message":"retired"}}`))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(time.Hour)}, "")
	if err := c.AuthGet(context.Background(), "/v1/organizations", nil, nil); !errors.Is(err, auth.ErrSignInRetired) {
		t.Fatalf("err = %v", err)
	}
	if !auth.Retired(c.dir, config.DefaultProfile) {
		t.Fatal("the stored sign-in was not marked retired")
	}
	err := c.AuthGet(context.Background(), "/v1/organizations", nil, nil)
	if !errors.Is(err, auth.ErrSignInRetired) || !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("a second request = %v, want retired and signed out", err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests: the retired token was presented again", n)
	}
}
