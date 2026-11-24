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

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
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

func signInMachine(t *testing.T, account string, signIn *fakeSignIn, teamIDs ...string) (*routedMachine, Router, map[string]string) {
	t.Helper()
	m := newRoutedMachine(t)
	teams := make([]routing.Team, 0, len(teamIDs))
	for _, id := range teamIDs {
		tm := orgTeam("org-a", id, listed...)
		tm.Policy.IncludePrompts, tm.Policy.IncludeToolContent = false, false
		teams = append(teams, tm)
	}
	enr := m.createState(config.DefaultProfile, account, teams, func(s *routing.State, _ map[string]string) error {
		return s.Bind(app.Origin, "org-a", false)
	})
	r := m.router(signIn)
	r.OTLPPinned = true
	return m, r, enr
}

func eventFor(enrollment, session, account string) spool.Event {
	return spool.Event{Repository: app, Name: semconv.TermaSessionQuotaEvent, SessionID: session, Account: account,
		Profile: config.DefaultProfile, Enrollment: enrollment, Attrs: map[string]any{}}
}

// On a person's profile, events leave with the sign-in's token, naming their team, and
// only under the account they were collected under: another person's backlog is held,
// never sent in this person's name. An event that names no lane is dropped, never sent.
func TestEventsLeaveOnlyUnderTheAccountThatCollectedThem(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	signIn := &fakeSignIn{account: aliceLane, token: "token-a"}
	m, r, enr := signInMachine(t, aliceLane, signIn, "p1", "p2")
	m.queueRaw(
		eventFor(enr["p1"], "mine", aliceLane),
		eventFor(enr["p1"], "someone-elses", bobLane),
		eventFor(enr["p2"], "before-user-tokens", ""))
	res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, OTLPURL: srv.URL}, true, 0)
	if res.Sent != 1 || res.Held != 1 || res.Dropped != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	for _, req := range sent() {
		if req.auth != "Bearer token-a" || req.project == "" || !strings.Contains(req.body, `"stringValue":"`+req.project+`"`) {
			t.Errorf("sent with %q naming %q", req.auth, req.project)
		}
		if strings.Contains(req.body, "someone-elses") || strings.Contains(req.body, "before-user-tokens") {
			t.Error("another person's event, or no one's, left under this sign-in")
		}
	}
	if n, _, _ := m.spool.Pending(); n != 1 {
		t.Fatalf("%d events queued, want only the other person's kept", n)
	}
}

// Signed out, events are held, not failed: nothing about them is wrong.
func TestEventsWaitForASignIn(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	m, r, enr := signInMachine(t, aliceLane, &fakeSignIn{}, "p1")
	m.queueRaw(eventFor(enr["p1"], "mine", aliceLane))
	res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, OTLPURL: srv.URL}, true, 0)
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
		signIn := &fakeSignIn{account: aliceLane, token: "token-a"}
		m, r, enr := signInMachine(t, aliceLane, signIn, "p1")
		m.queueRaw(eventFor(enr["p1"], "mine", aliceLane))
		res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, OTLPURL: srv.URL}, true, 0)
		srv.Close()
		if res.Sent != 0 || len(res.Failures) != 1 {
			t.Fatalf("%d: Flush = %+v", tc.status, res)
		}
		if n, _, _ := m.spool.Pending(); n != 1 {
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
// person's, nor one that names no lane, which is dropped.
func TestAServerKeyNeverSendsAPersonsEvents(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	m, r, enr := signInMachine(t, sendas.NoPersonLane, &fakeSignIn{}, "p1")
	if err := keystore.Set(m.config, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	keys := eventFor(enr["p1"], "the-keys", "")
	keys.NoPerson = true
	m.queueRaw(eventFor(enr["p1"], "a-persons", aliceLane), keys, eventFor(enr["p1"], "older-termas", ""))
	res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, ServerKeyAuthURL: routedAuth, OTLPURL: srv.URL, ServerKeySignIn: true, Team: "p1"}, true, 0)
	if res.Sent != 1 || res.Held != 1 || res.Dropped != 1 {
		t.Fatalf("Flush = %+v", res)
	}
	for _, req := range sent() {
		if strings.Contains(req.body, "a-persons") || strings.Contains(req.body, "older-termas") {
			t.Fatal("a person's event left with the server key")
		}
		if req.project != "" {
			t.Fatalf("server key sent X-Mirador-Project = %q, want empty", req.project)
		}
	}
}

// Events sent with a person's token ask who they name for that token's account, the one
// they were collected under, never the account signed in when the user is asked.
func TestEventsAskForTheUserOfTheAccountWhoseTokenSendsThem(t *testing.T) {
	t.Parallel()
	srv, _ := recordingIngest(t, http.StatusOK)
	m, r, enr := signInMachine(t, aliceLane, &fakeSignIn{account: aliceLane, token: "token-a"}, "p1")
	var asked []string
	r.EnrolledUser = func(_ string, _ routing.Team, _, account string) string { asked = append(asked, account); return "" }
	m.queueRaw(eventFor(enr["p1"], "mine", aliceLane))
	if res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, OTLPURL: srv.URL}, true, 0); res.Sent != 1 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if !slices.Equal(asked, []string{aliceLane}) {
		t.Fatalf("asked for the user of %q, want A's account", asked)
	}
}

// Events no credential here may send never read routing state: another person's events
// wait untouched under this sign-in.
func TestEventsThatCantLeaveAskForNoRoutingState(t *testing.T) {
	t.Parallel()
	srv, sent := recordingIngest(t, http.StatusOK)
	m, r, enr := signInMachine(t, aliceLane, &fakeSignIn{account: aliceLane, token: "token-a"}, "team-b")
	asked := 0
	r.loadState = func(dir, profile string) (routing.State, bool, error) {
		asked++
		return routing.Load(dir, profile)
	}
	m.queueRaw(eventFor(enr["team-b"], "someone-elses", bobLane))
	res := r.Flush(t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, AuthURL: routedAuth, OTLPURL: srv.URL}, true, 0)
	if res.Sent != 0 || res.Held != 1 || len(sent()) != 0 {
		t.Fatalf("Flush = %+v", res)
	}
	if asked != 0 {
		t.Fatalf("loaded routing state %d times for events that can't leave", asked)
	}
}
