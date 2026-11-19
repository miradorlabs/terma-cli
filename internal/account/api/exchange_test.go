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

// expired is a credential whose access token has expired, so a Refresh redeems.
func expired() *auth.Credential {
	return &auth.Credential{AccessToken: "mir_cli_stale", RefreshToken: "mir_clr_stale", SessionID: "session-1",
		ExpiresAt: time.Now().Add(-time.Hour)}
}

// markStored marks the stored credential's refresh token as sent at at without an answer.
func markStored(t *testing.T, client *Client, at time.Time) {
	t.Helper()
	if err := auth.Locked(context.Background(), client.dir, func(tx *auth.Tx) error {
		return tx.MarkUnsettled(config.DefaultProfile, "org-test", "mir_clr_stale", at)
	}); err != nil {
		t.Fatal(err)
	}
}

// A refresh that runs out of budget without an answer leaves its token expired for good,
// so no process sends it again.
func TestClient_AnUnsettledRefreshExpiresItsToken(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, badGateway)
	client := newTestClient(t, srv.URL, expired(), "")
	quickRetries(client, 10)

	if err := client.Refresh(context.Background()); !errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want an unknown outcome", err)
	}
	if mark := storedFor(t, client).UnsettledSince(); !mark.Equal(auth.UnsettledExpired) {
		t.Fatalf("stored mark = %v, want it expired", mark)
	}
	sent := len(log.tokens())

	log.answer.Store(func(w http.ResponseWriter) { rotatedPair(w) })
	other, err := New(&config.Config{Dir: client.dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
		Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Refresh(context.Background()); !errors.Is(err, errRefreshUnsettled) {
		t.Fatalf("second process's Refresh = %v, want a sign-in to redo", err)
	}
	if got := len(log.tokens()); got != sent {
		t.Errorf("%d refresh tokens sent after the budget; want none", got-sent)
	}
}

// A process killed with its refresh unanswered leaves its mark. Another process within the
// budget sends the same token again and stores the answer, which has no mark.
func TestClient_AKilledRefreshIsResentWithinItsBudget(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, func(w http.ResponseWriter) { rotatedPair(w) })
	client := newTestClient(t, srv.URL, expired(), "")
	markStored(t, client, time.Now().Round(0).Add(-time.Second))

	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if want := []string{"mir_clr_stale"}; !slices.Equal(log.tokens(), want) {
		t.Errorf("refresh tokens sent = %q, want %q", log.tokens(), want)
	}
	stored := storedFor(t, client)
	if stored.RefreshToken != "mir_clr_rotated" || !stored.RefreshUnsettledAt.IsZero() {
		t.Errorf("stored %q marked %v; want the rotated pair, unmarked", stored.RefreshToken, stored.RefreshUnsettledAt)
	}
}

// A mark another process left in budget, as one killed with its refresh outstanding
// does, makes the outcome unknown from the first answer on: that process's send may have
// rotated the pair. So the gateway's refusal is sent once more, as a lost race may draw
// it, and its 503 UNAVAILABLE does not settle the exchange, which spent nothing of this
// send but says nothing of the earlier one. Either way the answer after it is saved.
func TestClient_AnInheritedUnknownOutcomeIsNotSettledByTheFirstAnswer(t *testing.T) {
	t.Parallel()
	for name, first := range map[string]func(http.ResponseWriter){
		"401 UNAUTHENTICATED": func(w http.ResponseWriter) { gatewayError(w, http.StatusUnauthorized, "UNAUTHENTICATED") },
		"503 UNAVAILABLE":     func(w http.ResponseWriter) { gatewayError(w, http.StatusServiceUnavailable, "UNAVAILABLE") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var n atomic.Int32
			log, srv := newTokenLog(t, func(w http.ResponseWriter) {
				if n.Add(1) == 1 {
					first(w)
					return
				}
				rotatedPair(w)
			})
			client := newTestClient(t, srv.URL, expired(), "")
			quickRetries(client, 20)
			markStored(t, client, time.Now().Round(0).Add(-testUnit))

			if err := client.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if want := []string{"mir_clr_stale", "mir_clr_stale"}; !slices.Equal(log.tokens(), want) {
				t.Errorf("refresh tokens sent = %q, want %q: the same token again", log.tokens(), want)
			}
			wantRotatedPair(t, client)
			if mark := storedFor(t, client).RefreshUnsettledAt; !mark.IsZero() {
				t.Errorf("stored mark = %v, want none on the saved pair", mark)
			}
		})
	}
}

// A token whose refresh got no answer longer ago than the budget is never sent: the
// server no longer forgives it. Neither is one marked in the future, after the wall clock
// went back, even once the clock comes round to its mark again: the verdict is final. The
// credential stays, since it may be alive.
func TestClient_AnUnsettledRefreshPastItsBudgetSendsNothing(t *testing.T) {
	t.Parallel()
	for name, mark := range map[string]time.Duration{
		"past the budget": -2 * RefreshBudget,
		"in the future":   time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log, srv := newTokenLog(t, func(w http.ResponseWriter) { rotatedPair(w) })
			client := newTestClient(t, srv.URL, expired(), "")
			at := time.Now().Round(0).Add(mark)
			markStored(t, client, at)

			err := client.Refresh(context.Background())
			if !errors.Is(err, auth.ErrNotLoggedIn) || !strings.Contains(err.Error(), "run `terma setup`") {
				t.Fatalf("Refresh = %v, want a sign-in to redo", err)
			}
			stored := storedFor(t, client)
			if stored.RefreshToken != "mir_clr_stale" || !stored.UnsettledSince().Equal(auth.UnsettledExpired) {
				t.Errorf("stored %q marked %v; want the credential kept, expired", stored.RefreshToken, stored.UnsettledSince())
			}

			// The wall clock comes round to the mark: still nothing is sent, by this
			// process or another.
			again, err := New(&config.Config{Dir: client.dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
				Options{Version: "test"})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range []*Client{client, again} {
				c.now = func() time.Time { return at.Add(5 * time.Second) }
				if err := c.Refresh(context.Background()); !errors.Is(err, errRefreshUnsettled) {
					t.Errorf("Refresh at the mark = %v, want a sign-in to redo", err)
				}
			}
			if got := log.tokens(); len(got) != 0 {
				t.Errorf("refresh tokens sent = %q, want none", got)
			}
		})
	}
}

// The budget runs on the wall clock. A Mac asleep between two attempts stops Go's
// monotonic clock, not the server's grace: the resend is not sent, and the token expires.
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
	client.pauseUnit = testUnit
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
	if mark := storedFor(t, client).UnsettledSince(); !mark.Equal(auth.UnsettledExpired) {
		t.Errorf("stored mark = %v, want it expired", mark)
	}
}

// The mark is written before the token is sent, so a process killed with the answer
// outstanding leaves it too. An answer that settles the exchange takes it away: a pair
// replaces the marked one, and a definite refusal to refresh, or a 503, shows nothing
// was spent.
func TestClient_TheMarkPrecedesTheSendAndGoesWithAnAnswer(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(http.ResponseWriter){
		"pair": func(w http.ResponseWriter) { rotatedPair(w) },
		"403":  func(w http.ResponseWriter) { gatewayError(w, http.StatusForbidden, "PERMISSION_DENIED") },
		"429":  func(w http.ResponseWriter) { gatewayError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED") },
		"503":  func(w http.ResponseWriter) { gatewayError(w, http.StatusServiceUnavailable, "UNAVAILABLE") },
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
	if err := client.markUnsettled(&auth.Credential{RefreshToken: "mir_clr_stale"}, mark, nil); err != nil {
		t.Fatal(err)
	}

	if err := client.Refresh(context.Background()); !errors.Is(err, errRefreshUnsettled) {
		t.Fatalf("Refresh = %v, want a sign-in to redo", err)
	}
	if got := log.tokens(); len(got) != 0 {
		t.Errorf("refresh tokens sent = %q, want none", got)
	}
	if got := storedFor(t, client).UnsettledSince(); !got.Equal(auth.UnsettledExpired) {
		t.Errorf("stored mark = %v, want the client's verdict put back, expired", got)
	}
}

// Server errors for a while do not end an exchange whose outcome is unknown: it
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
	// Room for a fourth attempt, 7 units in, with plenty to spare.
	quickRetries(client, 20)

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

// A refresh whose TLS handshake fails, as on a captive portal whose certificate the client
// does not trust, never had a connection and sent nothing: it is not sent again and leaves
// no mark. A 502 came over a connection, so its outcome is unknown and it is sent again
// (TestClient_RetriesARefreshWithAnUnknownOutcome).
func TestClient_ATLSFailureSpentNothing(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		rotatedPair(w)
	}))
	defer srv.Close()
	// The client does not trust the test server's certificate.
	client := newTestClient(t, srv.URL, expired(), "")

	start := time.Now()
	err := client.Refresh(context.Background())
	if err == nil || errors.Is(err, auth.ErrNotLoggedIn) || errors.As(err, new(unsettledError)) {
		t.Fatalf("Refresh = %v, want a plain error", err)
	}
	if elapsed := time.Since(start); elapsed >= client.pause(0) {
		t.Errorf("Refresh took %v; want no pause before a retry", elapsed)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests reached the server", n)
	}
	if mark := storedFor(t, client).RefreshUnsettledAt; !mark.IsZero() {
		t.Errorf("stored mark = %v, want none", mark)
	}
}

// A process that loaded the credential before another removed it, on the server's
// refusal, sends nothing: the store no longer holds the session.
func TestClient_ARemovedCredentialIsNotSentByAnotherProcess(t *testing.T) {
	t.Parallel()
	log, srv := newTokenLog(t, func(w http.ResponseWriter) { gatewayError(w, http.StatusUnauthorized, "UNAUTHENTICATED") })
	first := newTestClient(t, srv.URL, expired(), "")
	second, err := New(&config.Config{Dir: first.dir, ProfileName: config.DefaultProfile, APIURL: srv.URL, AuthURL: srv.URL},
		Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("first Refresh = %v, want the session expired", err)
	}
	if err := second.Refresh(context.Background()); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("second Refresh = %v, want the session expired", err)
	}
	if got := log.tokens(); len(got) != 1 {
		t.Errorf("refresh tokens sent = %q, want the first process's only", got)
	}
}

// Refused is true only for the auth gateway's own refusal of the refresh token: a bare
// 401 from some other hop, or a session another sign-in replaced, is no refusal of it.
func TestRefusedIsOnlyTheGatewaysRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		answer     func(http.ResponseWriter)
		superseded bool
		want       bool
	}{
		{name: "the gateway's refusal", want: true, answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHENTICATED","message":"invalid or expired CLI token"}}`))
		}},
		{name: "a bare 401", answer: func(w http.ResponseWriter) { http.Error(w, "Unauthorized", http.StatusUnauthorized) }},
		{name: "a session another sign-in replaced", superseded: true, answer: rotatedPair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newTokenLog(t, tc.answer)
			client := newTestClient(t, srv.URL, &auth.Credential{AccessToken: "mir_cli_old", RefreshToken: "mir_clr_old",
				SessionID: "session-1", ExpiresAt: time.Now().Add(-time.Minute)}, "")
			if tc.superseded {
				if err := auth.UpdateCredential(client.dir, config.DefaultProfile, &auth.Credential{AccessToken: "mir_cli_new",
					RefreshToken: "mir_clr_new", SessionID: "session-2", ExpiresAt: time.Now().Add(time.Hour),
					OrganizationID: "org-test", AuthURL: srv.URL}); err != nil {
					t.Fatal(err)
				}
			}
			err := client.Refresh(context.Background())
			if !errors.Is(err, auth.ErrNotLoggedIn) || Refused(err) != tc.want {
				t.Fatalf("Refresh = %v, Refused = %v; want ErrNotLoggedIn, Refused %v", err, Refused(err), tc.want)
			}
		})
	}
}
