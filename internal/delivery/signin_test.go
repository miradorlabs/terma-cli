package delivery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// fakeSignIn is a sign-in whose account is account and whose token is token.
type fakeSignIn struct {
	mu       sync.Mutex
	account  string
	token    string
	refused  []string
	retired  bool
	accounts []string
}

func (f *fakeSignIn) Account() string { return f.account }

func (f *fakeSignIn) Token(_ context.Context, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts = append(f.accounts, account)
	if account != f.account {
		return "", nil
	}
	return f.token, nil
}

func (f *fakeSignIn) Refused(_ context.Context, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = append(f.refused, token)
}

func (f *fakeSignIn) Retired(string) { f.retired = true }

// sentRequest is what the ingest received.
type sentRequest struct{ auth, project, body string }

func recordingIngest(t *testing.T, status int) (*httptest.Server, func() []sentRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, sentRequest{r.Header.Get("Authorization"), r.Header.Get(sendas.ProjectHeader), string(b)})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []sentRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]sentRequest(nil), got...)
	}
}

func signInRouter(signIn *fakeSignIn) Router {
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, FetchedAt: time.Now()}
	return Router{OTLPPinned: true, SignIn: signIn,
		Policy: func(context.Context, *config.Config, string) (config.Policy, error) { return pol, nil }}
}

func spoolOf(t *testing.T, events ...spool.Event) *spool.Spool {
	t.Helper()
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(events...); err != nil {
		t.Fatal(err)
	}
	return s
}

func eventFor(project, session, account string) spool.Event {
	return spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: session, Account: account,
		Attrs: map[string]any{hookrun.AttrProjectID: project}}
}

// On a person's profile, events leave with the sign-in's token, naming their team, and
// only under the account they were collected under: another person's backlog is held,
// never sent in this person's name. An event that names no account, an older terma's no
// one sign-in may send, never leaves.
func TestEventsLeaveOnlyUnderTheAccountThatCollectedThem(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	signIn := &fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"}
	s := spoolOf(t,
		eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"),
		eventFor("p1", "someone-elses", "bbbbbbbbbbbbbbbb"),
		eventFor("p2", "before-user-tokens", ""))
	res := signInRouter(signIn).Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 1 || res.Held != 2 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	for _, r := range sent() {
		if r.auth != "Bearer token-a" || r.project == "" || !strings.Contains(r.body, `"stringValue":"`+r.project+`"`) {
			t.Errorf("sent with %q naming %q", r.auth, r.project)
		}
		if strings.Contains(r.body, "someone-elses") || strings.Contains(r.body, "before-user-tokens") {
			t.Error("another person's event, or no one's, left under this sign-in")
		}
	}
	if n, _, _ := s.Pending(); n != 2 {
		t.Fatalf("%d events queued, want the other person's and no one's kept", n)
	}

}

// Signed out, events are held, not failed: nothing about them is wrong.
func TestEventsWaitForASignIn(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	s := spoolOf(t, eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"))
	res := signInRouter(&fakeSignIn{}).Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 0 || res.Held != 1 || len(res.Failures) != 0 || len(sent()) != 0 {
		t.Fatalf("Flush = %+v, %d sent", res, len(sent()))
	}
}

// A refused token is handed back for rotation, and the events stay queued for the next
// pass; a retired sign-in is reported.
func TestARefusedTokenIsRotatedAndKeepsTheEvents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status  int
		body    string
		retired bool
	}{
		{http.StatusUnauthorized, "", false},
		{http.StatusForbidden, `{"code":7,"message":"retired","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`, true},
		// Retired is retired, whatever the status: never handed back as expired.
		{http.StatusUnauthorized, `{"code":16,"message":"retired","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		signIn := &fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"}
		s := spoolOf(t, eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"))
		res := signInRouter(signIn).Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
		srv.Close()
		if res.Sent != 0 || len(res.Failures) != 1 {
			t.Fatalf("%d: Flush = %+v", tc.status, res)
		}
		if n, _, _ := s.Pending(); n != 1 {
			t.Fatalf("%d: the event was not kept", tc.status)
		}
		if wantRefused := tc.status == http.StatusUnauthorized && !tc.retired; wantRefused != (len(signIn.refused) == 1 && signIn.refused[0] == "token-a") || !wantRefused && len(signIn.refused) != 0 {
			t.Fatalf("%d: refused tokens = %v", tc.status, signIn.refused)
		}
		if signIn.retired != tc.retired {
			t.Fatalf("%d: retired = %v", tc.status, signIn.retired)
		}
	}
}

// A profile set up with a server key sends only events no person collected: never a
// person's, nor one that names no account, an older terma's no one sign-in may send.
func TestAServerKeyNeverSendsAPersonsEvents(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	dir := t.TempDir()
	if err := keystore.Set(dir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	keys := eventFor("p1", "the-keys", "")
	keys.NoPerson = true
	// One collected while the sign-ins couldn't be read is maybe a person's: never no one's.
	s := spoolOf(t, eventFor("p1", "a-persons", "aaaaaaaaaaaaaaaa"), keys, eventFor("p1", "older-termas", ""), eventFor("p1", "unreadable", sendas.AccountUnreadable))
	r := signInRouter(&fakeSignIn{})
	r.ConfigDir = dir
	res := r.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL, ServerKeySignIn: true}, true, 0)
	if res.Sent != 1 || res.Held != 3 {
		t.Fatalf("Flush = %+v", res)
	}
	for _, req := range sent() {
		if strings.Contains(req.body, "a-persons") || strings.Contains(req.body, "older-termas") || strings.Contains(req.body, `"unreadable"`) {
			t.Fatal("a person's event left with the server key")
		}
	}
}

// Events sent with a person's token ask who they name for that token's account, the one
// they were collected under, never the account signed in when the user is asked.
func TestEventsAskForTheUserOfTheAccountWhoseTokenSendsThem(t *testing.T) {
	t.Parallel()
	srv, _ := recordingIngest(t, http.StatusOK)
	r := signInRouter(&fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"})
	var asked []string
	r.User = func(_ *config.Config, _, _, account string) string { asked = append(asked, account); return "" }
	if res := r.Flush(t.Context(), spoolOf(t, eventFor("p1", "mine", "aaaaaaaaaaaaaaaa")), &config.Config{OTLPURL: srv.URL}, true, 0); res.Sent != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if !slices.Equal(asked, []string{"aaaaaaaaaaaaaaaa"}) {
		t.Fatalf("asked for the user of %q, want A's account", asked)
	}
}

// Events no credential here may send ask for no policy: another person's team's is never
// fetched, or overwritten, under this sign-in.
func TestEventsThatCantLeaveAskForNoPolicy(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	r := signInRouter(&fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"})
	asked := 0
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, FetchedAt: time.Now()}
	r.Policy = func(context.Context, *config.Config, string) (config.Policy, error) { asked++; return pol, nil }
	res := r.Flush(t.Context(), spoolOf(t, eventFor("team-b", "someone-elses", "bbbbbbbbbbbbbbbb")), &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 0 || res.Held != 1 || len(sent()) != 0 {
		t.Fatalf("Flush = %+v", res)
	}
	if asked != 0 {
		t.Fatalf("asked for %d policies for events that can't leave", asked)
	}
}

// Events bound to the unreadable account are held, even while the sign-in in place reads as
// unreadable too: no policy is asked for, nothing is withheld, and no token is asked for.
func TestUnreadableEventsAreHeldWhileTheSignInIsUnreadable(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	signIn := &fakeSignIn{account: sendas.AccountUnreadable, token: "token-x"}
	r := signInRouter(signIn)
	asked := 0
	r.Policy = func(context.Context, *config.Config, string) (config.Policy, error) {
		asked++
		return config.Policy{}, nil
	}
	res := r.Flush(t.Context(), spoolOf(t, eventFor("p1", "unread", sendas.AccountUnreadable)), &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 0 || res.Held != 1 || res.Withheld != 0 || asked != 0 || len(sent()) != 0 || len(signIn.accounts) != 0 {
		t.Fatalf("Flush = %+v, %d policies, %d sent, tokens for %v; want it held untouched", res, asked, len(sent()), signIn.accounts)
	}
}
