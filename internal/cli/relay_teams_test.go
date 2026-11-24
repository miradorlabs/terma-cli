package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func routedTeam(enrollment, teamID, orgID, authURL string, personal bool, now time.Time) routing.Team {
	pol := config.Policy{
		Mode:           config.ModeRepo,
		TeamID:         teamID,
		OrganizationID: orgID,
		AuthURL:        authURL,
		Revision:       1,
		FetchedAt:      now,
	}
	if personal {
		pol.Mode = config.ModeGlobal
		pol.DefaultProjectID = teamID
	}
	return routing.Team{
		EnrollmentID:   enrollment,
		TeamID:         teamID,
		OrganizationID: orgID,
		AuthURL:        authURL,
		Personal:       personal,
		Epoch:          1,
		Policy:         pol,
	}
}

func saveRoutingState(t *testing.T, stateDir string, st routing.State) {
	t.Helper()
	if _, err := routing.Replace(stateDir, st.Profile, nil, st.Account, func(s *routing.State) error {
		s.Teams = st.Teams
		return nil
	}); err != nil {
		t.Fatalf("routing.Replace: %v", err)
	}
}

// Routed heartbeats prefer Personal first, skip HTTP 403 ORG_NOT_ACCESSIBLE to the next
// Active team without retiring or refreshing the sign-in or duplicating terma.user.id, and
// fail closed when routing state belongs to another account or is corrupt.
func TestTeamHeartbeatPrefersPersonalSkips403AndFailsClosed(t *testing.T) {
	type seenBeat struct {
		auth    string
		project string
		users   []string
	}
	var mu sync.Mutex
	var seen []seenBeat
	refuseProject := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var beat logspb.LogsData
		if err := proto.Unmarshal(body, &beat); err != nil {
			t.Error(err)
		}
		var users []string
		for _, kv := range beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
			if kv.Key == semconv.TermaUserIDKey {
				users = append(users, kv.Value.GetStringValue())
			}
		}
		proj := r.Header.Get(relay.ProjectHeader)
		mu.Lock()
		seen = append(seen, seenBeat{auth: r.Header.Get("Authorization"), project: proj, users: users})
		deny := refuseProject
		mu.Unlock()
		if proj == deny {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := &fakeAuth{srv: srv}
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	account := auth.AccountKey(srv.URL, "u-1")
	st := routing.State{
		Profile: config.DefaultProfile,
		Account: account,
		// Personal sits last in routing.State.Teams (per routing invariant), but heartbeat must try it first.
		Teams: []routing.Team{
			routedTeam("e-org", "team-org", "org-a", srv.URL, false, now),
			routedTeam("e-personal", "team-personal", "org-p", srv.URL, true, now),
		},
	}
	saveRoutingState(t, testApp.stateDir, st)

	opts := testHeartbeatOpts(t)
	// 1. Personal is preferred first when both are Active.
	if err := opts.HeartbeatSend(context.Background(), emptyBeat()); err != nil {
		t.Fatalf("routed heartbeat: %v", err)
	}
	if len(seen) != 1 || seen[0].project != "team-personal" || seen[0].auth != "Bearer "+fakeUserToken("u-1") || strings.Join(seen[0].users, ",") != "u-1" {
		t.Fatalf("first beat = %+v; want team-personal with single user u-1", seen)
	}

	// 2. HTTP 403 ORG_NOT_ACCESSIBLE on Personal skips to team-org without retiring token or duplicating terma.user.id.
	mu.Lock()
	seen = nil
	refuseProject = "team-personal"
	mu.Unlock()
	if err := opts.HeartbeatSend(context.Background(), emptyBeat()); err != nil {
		t.Fatalf("fallback heartbeat after 403 ORG_NOT_ACCESSIBLE: %v", err)
	}
	if len(seen) != 2 || seen[0].project != "team-personal" || seen[1].project != "team-org" {
		t.Fatalf("fallback beats = %+v; want [team-personal, team-org]", seen)
	}
	if len(seen[1].users) != 1 || seen[1].users[0] != "u-1" {
		t.Fatalf("fallback beat terma.user.id = %v; want exactly [u-1]", seen[1].users)
	}
	// SignIn must still return the token (neither Refused nor Retired was called).
	if tok, err := opts.SignIn.Token(t.Context(), account); err != nil || tok != fakeUserToken("u-1") {
		t.Fatalf("SignIn.Token after 403 ORG_NOT_ACCESSIBLE = %q, %v; token must remain intact", tok, err)
	}

	// 3. Wrong State.Account fails closed with ErrNoKey.
	mu.Lock()
	seen = nil
	mu.Unlock()
	st.Account = auth.AccountKey(srv.URL, "u-other")
	saveRoutingState(t, testApp.stateDir, st)
	if err := opts.HeartbeatSend(context.Background(), emptyBeat()); !errors.Is(err, relay.ErrNoKey) || len(seen) != 0 {
		t.Fatalf("wrong-account beat = %v, seen=%+v; want ErrNoKey and 0 requests", err, seen)
	}

	// 4. Corrupt routing file fails closed (never falls back to legacy path).
	path, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := opts.HeartbeatSend(context.Background(), emptyBeat()); err == nil || len(seen) != 0 {
		t.Fatalf("corrupt-state beat = %v, seen=%+v; want error and 0 requests", err, seen)
	}
}

// On a server-key routed profile, heartbeats go with keys.json, send no X-Mirador-Project
// header, and stamp no terma.user.id.
func TestTeamHeartbeatServerKeySendsNoProjectOrUser(t *testing.T) {
	var gotAuth, gotProject string
	var gotUsers int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var beat logspb.LogsData
		if err := proto.Unmarshal(body, &beat); err != nil {
			t.Error(err)
		}
		for _, kv := range beat.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
			if kv.Key == semconv.TermaUserIDKey {
				gotUsers++
			}
		}
		gotAuth = r.Header.Get("Authorization")
		gotProject = r.Header.Get(relay.ProjectHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	authSandbox(t, &fakeAuth{srv: srv})
	serverKeyProfile(t)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.Team = "team-srv"
		p.ServerKeyAuthURL = srv.URL
	}); err != nil {
		t.Fatal(err)
	}
	srvKey := "ter_srv_" + "heartbeat0123456789"
	if err := keystore.Set(testApp.dir, "team-srv", srvKey, keystore.Hosts{OTLP: srv.URL, API: srv.URL}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	saveRoutingState(t, testApp.stateDir, routing.State{
		Profile: config.DefaultProfile,
		Account: sendas.NoPersonLane,
		Teams:   []routing.Team{routedTeam("e-srv", "team-srv", "org-a", srv.URL, false, now)},
	})

	opts := testHeartbeatOpts(t)
	if err := opts.HeartbeatSend(context.Background(), emptyBeat()); err != nil {
		t.Fatalf("server-key routed heartbeat: %v", err)
	}
	if gotAuth != "Bearer "+srvKey || gotProject != "" || gotUsers != 0 {
		t.Fatalf("server-key beat got auth=%q project=%q users=%d", gotAuth, gotProject, gotUsers)
	}
}
