package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	if !unavailable.Retryable() || unavailable.OrgNotAccessible() {
		t.Error("AUTHORIZATION_UNAVAILABLE is retryable and says nothing about access")
	}
	if !gone.OrgNotAccessible() || gone.Retryable() {
		t.Error("ORG_NOT_ACCESSIBLE is the loss of access")
	}
	if denied.OrgNotAccessible() {
		t.Error("a 403 without the reason is not the loss of access")
	}
}

// A refresh whose answer is lost (the connection cut after the request went out) may have
// rotated: the stored pair says so, and the next client retries the same token within the
// budget rather than treat it as fresh.
func TestALostRefreshAnswerLeavesTheRefreshInDoubt(t *testing.T) {
	var cut atomic.Bool
	cut.Store(true)
	script := &refreshScript{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cut.Load() && r.URL.Path == tokenPath {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		script.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "access-0", RefreshToken: "refresh-0",
		ExpiresAt: time.Now().Add(-time.Hour)}, "")
	if err := c.Refresh(context.Background()); err == nil || errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("err = %v, want the lost answer, the sign-in kept", err)
	}
	marked := storedPair(t, c)
	if marked.RefreshInDoubtSince.IsZero() || marked.RefreshToken != "refresh-0" {
		t.Fatalf("stored %+v, want refresh-0 marked in doubt", marked)
	}
	cut.Store(false)
	next := newTestClient(t, srv.URL, marked, "")
	next.dir = c.dir
	if err := next.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sent := script.sent(); len(sent) != 1 || sent[0] != "refresh-0" {
		t.Fatalf("presented %v, want the same token once more", sent)
	}
	if got := storedPair(t, c); !got.RefreshInDoubtSince.IsZero() {
		t.Fatal("the mark outlived the rotation it was for")
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

// An answer that rotated nothing clears the mark only when no attempt before it may have
// rotated: a 503 and then a 429 leave the token in doubt.
func TestALaterCleanAnswerKeepsAnEarlierDoubt(t *testing.T) {
	shortRetries(t, 10*time.Second, 10*time.Millisecond)
	script := &refreshScript{answers: []string{"503", "429"}}
	c := expiredClient(t, script)
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("a 429 was taken for a rotation")
	}
	if got := storedPair(t, c); got.RefreshInDoubtSince.IsZero() {
		t.Fatal("the 429 cleared the doubt the 503 left")
	}
}
