package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// tokenLog serves the token endpoint: answer says how, and it keeps the refresh tokens sent.
type tokenLog struct {
	mu     sync.Mutex
	sent   []string
	answer atomic.Value // func(http.ResponseWriter)
}

func newTokenLog(t *testing.T, answer func(http.ResponseWriter)) (*tokenLog, *httptest.Server) {
	t.Helper()
	l := &tokenLog{}
	l.answer.Store(answer)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		l.mu.Lock()
		l.sent = append(l.sent, req.RefreshToken)
		l.mu.Unlock()
		l.answer.Load().(func(http.ResponseWriter))(w)
	}))
	t.Cleanup(srv.Close)
	return l, srv
}

func (l *tokenLog) tokens() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sent)
}

func badGateway(w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) }

func storedFor(t *testing.T, client *Client) *auth.Credential {
	t.Helper()
	cred, err := auth.LoadCredentialFor(client.dir, config.DefaultProfile, "org-test")
	if err != nil {
		t.Fatalf("LoadCredentialFor: %v", err)
	}
	return cred
}

// A refresh whose answer never came marks the stored credential with its first send. A
// second process within the budget sends the same token again and stores the answer,
// which has no mark.
func TestClient_AnUnsettledRefreshIsMarkedAndResentWithinItsBudget(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, badGateway)
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	// One pause fits in this budget, so the exchange gives up after two attempts.
	client.exchangeBudget = refreshPause(0) + refreshPause(1)*3/4

	before := time.Now().Round(0)
	if err := client.Refresh(context.Background()); !errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want an unknown outcome", err)
	}
	mark := storedFor(t, client).RefreshUnsettledAt
	if mark.Before(before.Truncate(time.Second)) || mark.After(time.Now()) {
		t.Fatalf("stored mark = %v, want the first send, at about %v", mark, before)
	}
	if got := client.unsettledSince(client.Credential()); !got.Equal(mark) {
		t.Errorf("client mark = %v, want the stored %v", got, mark)
	}

	log.answer.Store(func(w http.ResponseWriter) { rotatedPair(w) })
	other, err := New(&config.Config{Dir: client.dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
		Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	other.exchangeBudget = client.exchangeBudget * 10
	if err := other.Refresh(context.Background()); err != nil {
		t.Fatalf("second process's Refresh: %v", err)
	}
	if want := []string{"mir_clr_stale", "mir_clr_stale", "mir_clr_stale"}; !slices.Equal(log.tokens(), want) {
		t.Errorf("refresh tokens sent = %q, want %q", log.tokens(), want)
	}
	stored := storedFor(t, other)
	if stored.RefreshToken != "mir_clr_rotated" || !stored.RefreshUnsettledAt.IsZero() {
		t.Errorf("stored %q marked %v; want the rotated pair, unmarked", stored.RefreshToken, stored.RefreshUnsettledAt)
	}
}

// A token whose refresh got no answer longer ago than the budget is never sent: the
// server no longer forgives it. The credential stays, since it may be alive.
func TestClient_AnUnsettledRefreshPastItsBudgetSendsNothing(t *testing.T) {
	t.Parallel()
	for name, mark := range map[string]time.Time{
		"past the budget": time.Now().Add(-2 * RefreshBudget),
		"in the future":   time.Now().Add(time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log, srv := newTokenLog(t, func(w http.ResponseWriter) { rotatedPair(w) })
			expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
				ExpiresAt: time.Now().Add(-time.Hour), RefreshUnsettledAt: mark.Round(0)}
			client := newTestClient(t, srv.URL, expired, "")

			err := client.Refresh(context.Background())
			if !errors.Is(err, auth.ErrNotLoggedIn) || !strings.Contains(err.Error(), "run `terma setup`") {
				t.Fatalf("Refresh = %v, want a sign-in to redo", err)
			}
			if got := log.tokens(); len(got) != 0 {
				t.Errorf("refresh tokens sent = %q, want none", got)
			}
			if stored := storedFor(t, client); stored.RefreshToken != "mir_clr_stale" {
				t.Errorf("stored %q, want the credential kept", stored.RefreshToken)
			}
		})
	}
}

// The budget runs on the wall clock. A Mac asleep between two attempts stops Go's
// monotonic clock, not the server's grace: the resend is not sent, and the token is marked.
func TestClient_AWallClockJumpStopsTheResend(t *testing.T) {
	t.Parallel()
	var asleep atomic.Bool
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		// The machine sleeps for an hour once the first attempt is on its way.
		asleep.Store(true)
		badGateway(w)
	})
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	start := time.Now().Round(0)
	client.now = func() time.Time {
		if asleep.Load() {
			return start.Add(time.Hour)
		}
		return start
	}

	if err := client.Refresh(context.Background()); !errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want an unknown outcome", err)
	}
	if got := log.tokens(); len(got) != 1 {
		t.Errorf("refresh tokens sent = %q, want the first only", got)
	}
	if mark := storedFor(t, client).RefreshUnsettledAt; !mark.Equal(start) {
		t.Errorf("stored mark = %v, want the first send, %v", mark, start)
	}
}

// The mark is written before the token is sent, so a process killed with the answer
// outstanding leaves it too. An answer that settles the exchange takes it away: a pair
// replaces the marked one, and a definite refusal to refresh shows nothing was spent.
func TestClient_TheMarkPrecedesTheSendAndGoesWithAnAnswer(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(http.ResponseWriter){
		"pair": func(w http.ResponseWriter) { rotatedPair(w) },
		"403":  func(w http.ResponseWriter) { gatewayError(w, http.StatusForbidden, "PERMISSION_DENIED") },
		"429":  func(w http.ResponseWriter) { gatewayError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var client *Client
			var markedOnArrival atomic.Bool
			_, srv := newTokenLog(t, func(w http.ResponseWriter) {
				// The holder of the lock is the client itself: reading the store is safe.
				cred, err := auth.LoadCredentialFor(client.dir, config.DefaultProfile, "org-test")
				markedOnArrival.Store(err == nil && !cred.RefreshUnsettledAt.IsZero())
				answer(w)
			})
			expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
				ExpiresAt: time.Now().Add(-time.Hour)}
			client = newTestClient(t, srv.URL, expired, "")

			_ = client.Refresh(context.Background())
			if !markedOnArrival.Load() {
				t.Error("the token reached the server before the store marked it")
			}
			if mark := storedFor(t, client).RefreshUnsettledAt; !mark.IsZero() {
				t.Errorf("stored mark = %v after a definite answer; want none", mark)
			}
			if mark := client.unsettledSince(&auth.Credential{RefreshToken: "mir_clr_stale"}); !mark.IsZero() {
				t.Errorf("client mark = %v after a definite answer; want none", mark)
			}
		})
	}
}

// A client that knows its token's refresh went unanswered keeps to that, even where the
// store lost the mark: past the budget it sends nothing.
func TestClient_ARefreshKeepsToItsOwnMark(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, func(w http.ResponseWriter) { rotatedPair(w) })
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	mark := time.Now().Round(0).Add(-2 * RefreshBudget)
	client.setUnsettled("mir_clr_stale", time.Time.IsZero, mark)

	if err := client.Refresh(context.Background()); !errors.Is(err, errRefreshUnsettled) {
		t.Fatalf("Refresh = %v, want a sign-in to redo", err)
	}
	if got := log.tokens(); len(got) != 0 {
		t.Errorf("refresh tokens sent = %q, want none", got)
	}
	if got := storedFor(t, client).RefreshUnsettledAt; !got.Equal(mark) {
		t.Errorf("stored mark = %v, want the client's %v put back", got, mark)
	}
}

// Server errors for a few seconds do not end an exchange whose outcome is unknown: it
// sends the token again until its budget is spent, so an answer that comes in time is
// saved and leaves no mark asking for a new sign-in.
func TestClient_ABurstOfServerErrorsLeavesNoMark(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		if n.Add(1) <= 3 {
			badGateway(w)
			return
		}
		rotatedPair(w)
	})
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	// Room for a fourth attempt, after pauses of 1, 2 and 4 s.
	client.exchangeBudget = refreshPause(0) + refreshPause(1) + refreshPause(2) + 2*time.Second

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := len(log.tokens()); got != 4 {
		t.Errorf("%d attempts; want 4", got)
	}
	if stored := storedFor(t, client); stored.RefreshToken != "mir_clr_rotated" || !stored.RefreshUnsettledAt.IsZero() {
		t.Errorf("stored %q marked %v; want the rotated pair, unmarked", stored.RefreshToken, stored.RefreshUnsettledAt)
	}
}

// A mark read without the credential lock may be gone by the time a client takes the lock:
// the process that wrote it got a definite answer and cleared it. Such a client sends its
// token as a fresh refresh, and a 429 then leaves nothing marked.
func TestClient_AMarkReadWithoutTheLockIsNotTrusted(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, func(w http.ResponseWriter) {
		gatewayError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED")
	})
	expired := &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
	client := newTestClient(t, srv.URL, expired, "")
	at := time.Now().Round(0).Truncate(time.Second)
	setMark := func(mark func(tx *auth.Tx) error) {
		t.Helper()
		if err := auth.Locked(context.Background(), client.dir, mark); err != nil {
			t.Fatal(err)
		}
	}
	// Another process marks the token and is mid-exchange when this one loads it...
	setMark(func(tx *auth.Tx) error {
		return tx.MarkUnsettled(config.DefaultProfile, "org-test", "mir_clr_stale", at)
	})
	loaded, err := New(&config.Config{Dir: client.dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
		Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// ...then gets a definite answer, and takes the mark back.
	setMark(func(tx *auth.Tx) error {
		return tx.ClearUnsettled(config.DefaultProfile, "org-test", "mir_clr_stale", at)
	})

	if err := loaded.Refresh(context.Background()); err == nil || errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want the 429 as it came", err)
	}
	if got := log.tokens(); len(got) != 1 {
		t.Errorf("refresh tokens sent = %q, want one", got)
	}
	if mark := storedFor(t, loaded).RefreshUnsettledAt; !mark.IsZero() {
		t.Errorf("stored mark = %v, want none", mark)
	}
}
