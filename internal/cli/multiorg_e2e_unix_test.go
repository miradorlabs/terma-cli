//go:build unix

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// keyedIngest stands in for the ingest host every team's records reach here (pinned
// with TERMA_OTLP_URL): it takes any key and keeps, per request, the key and the body.
type keyedIngest struct {
	*httptest.Server
	mu   sync.Mutex
	seen []ingested
}

type ingested struct{ key, body string }

func newKeyedIngest(t *testing.T) *keyedIngest {
	t.Helper()
	in := &keyedIngest{}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		in.mu.Lock()
		in.seen = append(in.seen, ingested{key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), body: string(body)})
		in.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(in.Close)
	return in
}

// keysFor are the keys of the requests that carried marker, each once.
func (in *keyedIngest) keysFor(marker string) []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	var keys []string
	for _, s := range in.seen {
		if strings.Contains(s.body, marker) && !strings.Contains(strings.Join(keys, "\n"), s.key) {
			keys = append(keys, s.key)
		}
	}
	return keys
}

// awaitKeysFor waits until a request carrying marker arrives, and returns the keys of those
// that did.
func (in *keyedIngest) awaitKeysFor(t *testing.T, marker string) []string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if keys := in.keysFor(marker); len(keys) > 0 {
			return keys
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing carrying %s reached the ingest", marker)
		}
	}
}

// gitCheckout is a new repository whose origin is github.com/<ownerRepo>.
func gitCheckout(t *testing.T, ownerRepo string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:" + ownerRepo + ".git"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// sessionInRepo is an agent session starting in root: its SessionStart hook, the built
// terma's `terma hook`, run by a shell that then becomes the agent's exporter and sends one
// log record of the session to the relay. The hook's claim names that shell, and the relay
// forwards only what the processes a claim names send it.
func sessionInRepo(t *testing.T, bin, root, sessionID, relayAddr, token string) {
	t.Helper()
	record, err := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.user_prompt"}},
		Attributes:   []*commonpb.KeyValue{{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sessionID}}}},
	}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(t.TempDir(), "logs.pb")
	if err := os.WriteFile(body, record, 0o600); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"` + sessionID + `","cwd":` + jsonString(root) + `,"hook_event_name":"SessionStart","source":"startup"}`
	sh := exec.Command("sh", "-c", `"$TERMA" hook --user session-start <<EOF
$PAYLOAD
EOF
exec curl -sSf -o /dev/null -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/x-protobuf" --data-binary @"$BODY" "http://$ADDR/v1/logs"`)
	sh.Dir = root
	sh.Env = append(os.Environ(), "TERMA="+bin, "PAYLOAD="+payload, "TOKEN="+token, "BODY="+body, "ADDR="+relayAddr)
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("the session in %s: %v\n%s", root, err, out)
	}
}

// signInFromTheBrowser answers the browser sign-in a running command started, as the
// approval page does: it reads the authorize URL from the command's stderr, has the fake
// auth host hand out a code for org, and sends that code to the command's loopback
// listener. It drains the rest of stderr into the returned buffer.
func signInFromTheBrowser(t *testing.T, f *fakeAuth, org string, stderr io.Reader) (approved <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		answered := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if answered || !strings.Contains(line, "/cli/auth?") {
				continue
			}
			answered = true
			u, err := url.Parse(line)
			if err != nil {
				done <- err
				continue
			}
			code := "browser-code-" + org[:8]
			f.browserGrant(code, org)
			resp, err := http.PostForm("http://127.0.0.1:"+u.Query().Get("port")+"/callback", url.Values{"state": {u.Query().Get("state")}, "code": {code}})
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}
		if !answered {
			done <- io.ErrUnexpectedEOF
		}
	}()
	return done
}

// End to end on a scratch machine, with httptest fakes for every network call: today's
// single-team machine is moved to routing by teams by the relay's own start; a team of a
// second organization is ticked, signing in to that organization, fetching its policy and
// minting its key; then an agent session in each organization's repository sends hook
// events through the spool and telemetry through the relay, and each reaches its own team
// with that team's own key, never the other's. An older terma finds no key and no sign-in.
func TestMultiOrgRoutingEndToEnd(t *testing.T) {
	for _, tool := range []string{"git", "sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	bin := termaBinary(t)
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	configDir, stateDir := t.TempDir(), t.TempDir()
	useDirs(t, configDir, stateDir)
	ingest := newKeyedIngest(t)
	for k, v := range map[string]string{
		"TERMA_CONFIG_DIR": configDir, "TERMA_STATE_DIR": stateDir, "TERMA_OTLP_URL": ingest.URL,
		"TERMA_POLICY_STUB": "", "TERMA_HOOKS": "", "CODEX_HOME": t.TempDir(), "COPILOT_HOME": t.TempDir(),
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": filepath.Join(t.TempDir(), "gitconfig"),
	} {
		t.Setenv(k, v)
	}
	f.orgs = []organization{{ID: orgA().ID, Name: "Acme", Type: "shared"}, {ID: orgB().ID, Name: "Beta Labs", Type: "shared"}}
	f.policyBodies = map[string]string{acmeAPI: policyBody("github.com/acme/api"), betaCore: policyBody("github.com/beta/core")}

	// Today's machine: one profile on Acme API, its validated policy, its sign-in to Acme
	// in credentials.json and its key in keys.json, and the relay set up for Claude Code.
	const acmeKey = "ter_srv_acme_from_before"
	if err := config.UpdateProfile(configDir, config.DefaultProfile, func(p *config.Profile) {
		p.OrganizationID, p.OrganizationName, p.Team, p.TeamName, p.Harnesses = orgA().ID, orgA().Name, acmeAPI, "Acme API", []string{"claude"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.WritePolicy(stateDir, config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/api"}, GitHooks: true,
		IncludePrompts: true, IncludeToolContent: true, TeamID: acmeAPI, OrganizationID: orgA().ID, AuthURL: f.srv.URL, Revision: 3,
		FetchedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(configDir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Set(configDir, acmeAPI, acmeKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "claude"); err != nil {
		t.Fatalf("relay setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	// The relay migrates as it starts. It runs as its own command line, as a relay is its own
	// process, beside the commands this test runs.
	relayApp := New(builtin.Agents, "dev")
	relayApp.binDirs = func() []string { return nil }
	relayApp.configure(configDir, stateDir)
	relayCmd := relayApp.NewRootCommand()
	relayCmd.PersistentPostRun = nil
	relayCmd.SetArgs([]string{"relay", "run", "--idle", "0", "--quiet"})
	relayCmd.SetOut(io.Discard)
	relayCmd.SetErr(io.Discard)
	ctx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	go func() { relayDone <- relayCmd.ExecuteContext(ctx) }()
	defer func() {
		stopRelay()
		if err := <-relayDone; err != nil {
			t.Errorf("relay run: %v", err)
		}
	}()
	awaitRelayRecord(t, claim.Dir(stateDir), 20*time.Second, relayDone)

	st, ok, err := routing.LoadReady(stateDir, config.DefaultProfile)
	if err != nil || !ok || len(st.Teams) != 1 || st.Teams[0].TeamID != acmeAPI || st.Bindings["github.com/acme/api"].Organization != orgA().ID {
		t.Fatalf("after the relay's start, the routing state = %+v, %v, %v", st, ok, err)
	}
	acme := st.Teams[0].EnrollmentID
	if key, _, err := keystore.GetTeam(configDir, acme); err != nil || key != acmeKey {
		t.Fatalf("Acme API's key in keys.v2.json = %q, %v", key, err)
	}

	// Beta Labs: no sign-in here yet. Ticking its team signs in through the browser,
	// fetches the team's policy and mints its key, with Beta Labs' own sign-in.
	stderr, stderrW := io.Pipe()
	approved := signInFromTheBrowser(t, f, orgB().ID, stderr)
	stdout, _, err := termaRun{within: 30 * time.Second, stderr: stderrW}.exec(t, "teams", "--org", "Beta Labs", "--select", "Beta Core", "--yes", "--no-browser", "--json")
	_ = stderrW.Close()
	if signInErr := <-approved; err != nil || signInErr != nil {
		t.Fatalf("teams --select Beta Core: %v (sign-in %v)\n%s", err, signInErr, stdout)
	}
	st, ok, err = routing.LoadReady(stateDir, config.DefaultProfile)
	if err != nil || !ok || len(st.Teams) != 2 || st.Teams[0].EnrollmentID != acme {
		t.Fatalf("after ticking Beta Core: %+v, %v, %v", st.Teams, ok, err)
	}
	beta := st.Teams[1]
	if beta.TeamID != betaCore || beta.OrganizationID != orgB().ID || st.Bindings["github.com/beta/core"].Organization != orgB().ID {
		t.Fatalf("Beta Core ticked as %+v, bindings %v", beta, st.Bindings)
	}
	betaKey, _, err := keystore.GetTeam(configDir, beta.EnrollmentID)
	if err != nil || betaKey == "" || betaKey == acmeKey {
		t.Fatalf("Beta Core's key = %q, %v", betaKey, err)
	}
	if cred, err := auth.LoadCredentialFor(configDir, config.DefaultProfile, orgB().ID); err != nil || cred.AccessToken != fakeToken(orgB().ID, "") {
		t.Fatalf("Beta Labs' sign-in = %+v, %v", cred, err)
	}

	// An agent session in each organization's repository.
	acmeRepo, betaRepo := gitCheckout(t, "acme/api"), gitCheckout(t, "beta/core")
	const acmeSession, betaSession = "e2e-acme-session", "e2e-beta-session"
	sessionInRepo(t, bin, acmeRepo, acmeSession, addr, token)
	sessionInRepo(t, bin, betaRepo, betaSession, addr, token)

	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := sp.Peek(100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{acmeSession: acme, betaSession: beta.EnrollmentID}
	for _, ev := range queued {
		if ev.SessionID != "" && ev.Enrollment != want[ev.SessionID] {
			t.Errorf("%s of %s spooled for enrollment %q, want %q", ev.Name, ev.SessionID, ev.Enrollment, want[ev.SessionID])
		}
	}
	if len(queued) == 0 {
		t.Fatal("the sessions' hooks spooled nothing")
	}
	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("spool flush: %v\n%s", err, out)
	}

	// Each session's hook events and telemetry reached its own team's key, and only it.
	for session, key := range map[string]string{acmeSession: acmeKey, betaSession: betaKey} {
		if keys := ingest.awaitKeysFor(t, session); len(keys) != 1 || keys[0] != key {
			t.Errorf("%s reached the ingest with keys %q, want only %q", session, keys, key)
		}
	}
	telemetry := func(session string) bool {
		ingest.mu.Lock()
		defer ingest.mu.Unlock()
		for _, s := range ingest.seen {
			if strings.Contains(s.body, session) && strings.Contains(s.body, "claude_code.user_prompt") {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(20 * time.Second); !telemetry(acmeSession) || !telemetry(betaSession); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the relay forwarded Acme's telemetry %v, Beta's %v", telemetry(acmeSession), telemetry(betaSession))
		}
	}
	for session, key := range map[string]string{acmeSession: acmeKey, betaSession: betaKey} {
		if keys := ingest.keysFor(session); len(keys) != 1 || keys[0] != key {
			t.Errorf("%s's telemetry and events reached the ingest with keys %q, want only %q", session, keys, key)
		}
	}

	// An older terma reads keys.json and credentials.json alone: both are empty, and
	// config.json names no team.
	var keys struct {
		Keys        map[string]json.RawMessage `json:"keys"`
		HarnessKeys map[string]json.RawMessage `json:"harness_keys"`
	}
	if b, err := os.ReadFile(keystore.FilePath(configDir)); err != nil || json.Unmarshal(b, &keys) != nil || len(keys.Keys) != 0 || len(keys.HarnessKeys) != 0 {
		t.Errorf("keys.json after the migration (%v):\n%s", err, b)
	}
	creds, err := os.ReadFile(config.CredentialsPath(configDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, secretOrOrg := range []string{"ter_cli", "ter_clr", orgA().ID, orgB().ID} {
		if bytes.Contains(creds, []byte(secretOrOrg)) {
			t.Errorf("credentials.json still holds %s:\n%s", secretOrOrg, creds)
		}
	}
	if file, err := config.LoadFile(configDir); err != nil || file.Profiles[config.DefaultProfile].Team != "" {
		t.Errorf("config.json still names a team: %v", err)
	}
}
