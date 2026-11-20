package delivery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

func signInRouter(signIn *fakeSignIn, legacy string) Router {
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, FetchedAt: time.Now()}
	// The older sign-ins were retired after the test's events were queued.
	return Router{OTLPPinned: true, SignIn: signIn, LegacyAccount: legacy, LegacyBefore: time.Now().Add(time.Hour),
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
// never sent in this person's name. Events an older terma queued count as the account its
// retired sign-ins belonged to.
func TestEventsLeaveOnlyUnderTheAccountThatCollectedThem(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	signIn := &fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"}
	s := spoolOf(t,
		eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"),
		eventFor("p1", "someone-elses", "bbbbbbbbbbbbbbbb"),
		eventFor("p2", "before-user-tokens", ""))
	res := signInRouter(signIn, "aaaaaaaaaaaaaaaa").Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Sent != 2 || res.Held != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	for _, r := range sent() {
		if r.auth != "Bearer token-a" || r.project == "" || !strings.Contains(r.body, `"stringValue":"`+r.project+`"`) {
			t.Errorf("sent with %q naming %q", r.auth, r.project)
		}
		if strings.Contains(r.body, "someone-elses") {
			t.Error("another person's event left under this sign-in")
		}
	}
	if n, _, _ := s.Pending(); n != 1 {
		t.Fatalf("%d events queued, want the other person's kept", n)
	}

	// Events bound to no account and queued after the older sign-ins were retired, with no
	// sign-in in place, leave under none.
	late := signInRouter(signIn, "aaaaaaaaaaaaaaaa")
	late.LegacyBefore = time.Now().Add(-time.Hour)
	s = spoolOf(t, eventFor("p2", "signed-out", ""))
	if res := late.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0); res.Sent != 0 || res.Held != 1 {
		t.Fatalf("Flush = %+v", res)
	}

	// Without a legacy account, an older terma's events wait for a server key.
	s = spoolOf(t, eventFor("p2", "before-user-tokens", ""))
	if res := signInRouter(signIn, "").Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0); res.Sent != 0 || res.Held != 1 {
		t.Fatalf("Flush = %+v", res)
	}
}

// Signed out, events are held, not failed: nothing about them is wrong.
func TestEventsWaitForASignIn(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	s := spoolOf(t, eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"))
	res := signInRouter(&fakeSignIn{}, "").Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
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
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		signIn := &fakeSignIn{account: "aaaaaaaaaaaaaaaa", token: "token-a"}
		s := spoolOf(t, eventFor("p1", "mine", "aaaaaaaaaaaaaaaa"))
		res := signInRouter(signIn, "").Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
		srv.Close()
		if res.Sent != 0 || len(res.Failures) != 1 {
			t.Fatalf("%d: Flush = %+v", tc.status, res)
		}
		if n, _, _ := s.Pending(); n != 1 {
			t.Fatalf("%d: the event was not kept", tc.status)
		}
		if tc.status == http.StatusUnauthorized && (len(signIn.refused) != 1 || signIn.refused[0] != "token-a") {
			t.Fatalf("refused tokens = %v", signIn.refused)
		}
		if signIn.retired != tc.retired {
			t.Fatalf("%d: retired = %v", tc.status, signIn.retired)
		}
	}
}

// A profile set up with a server key sends only events no person's sign-in collected.
func TestAServerKeyNeverSendsAPersonsEvents(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	dir := t.TempDir()
	if err := keystore.Set(dir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	old := eventFor("p1", "older-termas", "")
	old.Time = time.Now().Add(-2 * time.Hour)
	s := spoolOf(t, eventFor("p1", "a-persons", "aaaaaaaaaaaaaaaa"), old, eventFor("p1", "unbound", ""))
	r := signInRouter(&fakeSignIn{}, "bbbbbbbbbbbbbbbb")
	r.LegacyBefore = time.Now().Add(-time.Hour)
	r.ConfigDir = dir
	res := r.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL, ServerKeySignIn: true}, true, 0)
	if res.Sent != 1 || res.Held != 2 {
		t.Fatalf("Flush = %+v", res)
	}
	for _, req := range sent() {
		if strings.Contains(req.body, "a-persons") || strings.Contains(req.body, "older-termas") {
			t.Fatal("a person's event left with the server key")
		}
	}
}
