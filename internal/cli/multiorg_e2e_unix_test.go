//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// e2eIngest stands in for the Mirador ingest host (pinned via TERMA_OTLP_URL). It captures
// every request's bearer token, X-Mirador-Project header, and body, and can refuse specific
// team IDs with 403 ORG_NOT_ACCESSIBLE to exercise org-wide revocation recovery.
type e2eIngest struct {
	*httptest.Server
	mu          sync.Mutex
	seen        []e2eDelivery
	refuseTeams map[string]bool
	refused     map[string]int
}

type e2eDelivery struct {
	token   string
	project string
	path    string
	body    string
}

func newE2EIngest(t *testing.T) *e2eIngest {
	t.Helper()
	in := &e2eIngest{refuseTeams: map[string]bool{}, refused: map[string]int{}}
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		proj := r.Header.Get("X-Mirador-Project")
		in.mu.Lock()
		refused := in.refuseTeams[proj]
		if refused {
			in.refused[proj]++
		} else {
			in.seen = append(in.seen, e2eDelivery{
				token:   tok,
				project: proj,
				path:    r.URL.Path,
				body:    string(body),
			})
		}
		in.mu.Unlock()
		if refused {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":7,"message":"denied","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(in.Close)
	return in
}

func (in *e2eIngest) setRefused(teamIDs ...string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, id := range teamIDs {
		in.refuseTeams[id] = true
	}
}

func (in *e2eIngest) refusedCount(teamID string) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.refused[teamID]
}

func (in *e2eIngest) matches(marker string) []e2eDelivery {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []e2eDelivery
	for _, d := range in.seen {
		if strings.Contains(d.body, marker) {
			out = append(out, d)
		}
	}
	return out
}

func (in *e2eIngest) await(t *testing.T, marker string, want func(e2eDelivery) bool) e2eDelivery {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(40 * time.Millisecond) {
		for _, d := range in.matches(marker) {
			if want == nil || want(d) {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q at ingest (seen %d requests)", marker, len(in.matches(marker)))
		}
	}
}

func e2eGitCheckout(t *testing.T, ownerRepo string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "git@github.com:" + ownerRepo + ".git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func e2eOTLPLogBody(t *testing.T, sessionID, marker string, at time.Time) string {
	t.Helper()
	raw, err := proto.Marshal(&logspb.LogsData{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: uint64(at.UnixNano()),
					Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude_code.user_prompt"}},
					Attributes: []*commonpb.KeyValue{
						{Key: "session.id", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: sessionID}}},
						{Key: "prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: marker}}},
					},
				}},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "logs.pb")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runAgentSession runs a real built `terma hook --user session-start` in root and exports
// one OTLP log record from that same shell process so the relay verifies the claim PID.
func runAgentSession(t *testing.T, bin, root, sessionID, marker, relayAddr, relayToken string) {
	t.Helper()
	body := e2eOTLPLogBody(t, sessionID, marker, time.Now())
	payload, _ := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"cwd":             root,
		"hook_event_name": "SessionStart",
		"source":          "startup",
	})
	sh := exec.Command("sh", "-c", `"$TERMA" hook --user session-start <<EOF
$PAYLOAD
EOF
exec curl -sSf -o /dev/null -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/x-protobuf" --data-binary @"$BODY" "http://$ADDR/v1/logs"`)
	sh.Dir = root
	sh.Env = append(os.Environ(),
		"TERMA="+bin,
		"PAYLOAD="+string(payload),
		"TOKEN="+relayToken,
		"BODY="+body,
		"ADDR="+relayAddr,
	)
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("agent session in %s: %v\n%s", root, err, out)
	}
}

func startE2ERelay(t *testing.T, configDir, stateDir string) (stop func()) {
	t.Helper()
	relayApp := New(builtin.Agents, "dev")
	relayApp.binDirs = func() []string { return nil }
	relayApp.configure(configDir, stateDir)
	cmd := relayApp.NewRootCommand()
	cmd.PersistentPostRun = nil
	cmd.SetArgs([]string{"relay", "run", "--idle", "0", "--quiet"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	awaitRelayRecord(t, claim.Dir(stateDir), 20*time.Second, done)

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("relay run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestMultiOrgSingleTokenEndToEnd exercises the complete single-user-token multi-org stack
// on an isolated scratch machine:
//  1. Multi-org routing across two shared orgs (Acme, Beta Labs) + Personal on one user token.
//  2. Org-wide 403 ORG_NOT_ACCESSIBLE refusing every Acme enrollment via coalesced refresh nudge
//     without retiring Alice's user token, while Beta Labs and Personal keep delivering.
//  3. Account switch CAS (alice -> bob) rejecting stale alice writers and failing closed on
//     wrong-account routing.State in route explain and doctor.
func TestMultiOrgSingleTokenEndToEnd(t *testing.T) {
	for _, tool := range []string{"git", "sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	// Build terma before sandboxMachine moves HOME away from the Go build cache.
	bin := termaBinary(t)

	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	configDir, stateDir := t.TempDir(), t.TempDir()
	useDirs(t, configDir, stateDir)
	ingest := newE2EIngest(t)

	for k, v := range map[string]string{
		"TERMA_CONFIG_DIR":    configDir,
		"TERMA_STATE_DIR":     stateDir,
		"TERMA_OTLP_URL":      ingest.URL,
		"TERMA_POLICY_STUB":   "",
		"TERMA_HOOKS":         "",
		"CODEX_HOME":          t.TempDir(),
		"COPILOT_HOME":        t.TempDir(),
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "gitconfig"),
	} {
		t.Setenv(k, v)
	}

	var acmeRevoked atomic.Bool
	f.orgs = []organization{orgA(), orgB(), orgPersonal()}
	f.projects = map[string][]project{
		orgA().ID:        projectsIn(orgA().ID),
		orgB().ID:        projectsIn(orgB().ID),
		orgPersonal().ID: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: orgPersonal().ID}},
	}
	f.policyBodies = map[string]string{
		teamsAcmeWeb:      policyBody("github.com/acme/web"),
		acmeAPI:           policyBody("github.com/acme/api"),
		betaCore:          policyBody("github.com/beta/core"),
		teamsPersonalTeam: globalBody,
	}
	f.onPolicy = func(w http.ResponseWriter, r *http.Request) bool {
		if !acmeRevoked.Load() {
			return false
		}
		if team := r.URL.Query().Get("project_id"); team == acmeAPI || team == teamsAcmeWeb {
			orgNotAccessible(w)
			return true
		}
		return false
	}

	// 1. Sign in as alice via terma setup --desktop-grant for Acme API, then tick Acme Web,
	// Beta Core, and Personal on the SAME user token (zero extra sign-ins).
	const (
		aliceID = "11111111-aaaa-4aaa-8aaa-111111111111"
		bobID   = "22222222-bbbb-4bbb-8bbb-222222222222"
	)
	f.grant("code-alice", aliceID)
	stdin := identityLine(aliceID, orgA().ID) + `{"code":"code-alice"}` + "\n"
	if stdout, stderr, err := (termaRun{within: 30 * time.Second, stdin: strings.NewReader(stdin)}).exec(t,
		"setup", "-y", "--desktop-grant", "--insecure-storage", "--relay-service", "off",
		"--org", orgA().ID, "--team", acmeAPI, "--harness", "claude"); err != nil {
		t.Fatalf("alice setup: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if out, err := runTerma(t, "teams", "--select", "Acme Web", "--select", "Beta Core", "--select", "Personal"); err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if f.exchanges.Load() != 1 {
		t.Fatalf("token exchanges = %d, want 1 (single user token across all orgs)", f.exchanges.Load())
	}

	addr := freeAddr(t)
	if out, err := runTerma(t, "relay", "setup", "--no-start", "--addr", addr, "--harness", "claude"); err != nil {
		t.Fatalf("relay setup: %v\n%s", err, out)
	}
	relayToken, err := daemon.Token(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stopRelay := startE2ERelay(t, configDir, stateDir)
	info, ok := daemon.RunningRelay(claim.Dir(stateDir))
	if !ok || !info.Routing {
		t.Fatalf("relay RunInfo = %+v (ok=%v), want Routing=true", info, ok)
	}

	acmeRepo := e2eGitCheckout(t, "acme/api")
	betaRepo := e2eGitCheckout(t, "beta/core")
	personalRepo := e2eGitCheckout(t, "me/scratch")

	// Route explain matches each destination before sending.
	for repo, want := range map[string]string{
		acmeRepo:     "Goes to:   Acme",
		betaRepo:     "Goes to:   Beta Core",
		personalRepo: "Goes to:   Personal",
	} {
		out, err := runTerma(t, "route", "explain", repo)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("route explain %s (%v):\n%s\nwant %q", repo, err, out, want)
		}
	}

	// Send sessions in Acme, Beta Labs, and Personal; flush spool and verify isolation.
	aliceToken := fakeUserToken(aliceID)
	runAgentSession(t, bin, acmeRepo, "e2e-acme-1", "marker-acme-1", addr, relayToken)
	runAgentSession(t, bin, betaRepo, "e2e-beta-1", "marker-beta-1", addr, relayToken)
	runAgentSession(t, bin, personalRepo, "e2e-personal-1", "marker-personal-1", addr, relayToken)
	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("spool flush: %v\n%s", err, out)
	}
	for _, want := range []struct {
		session, marker, project string
	}{
		{"e2e-acme-1", "marker-acme-1", acmeAPI},
		{"e2e-beta-1", "marker-beta-1", betaCore},
		{"e2e-personal-1", "marker-personal-1", teamsPersonalTeam},
	} {
		// Verify OTLP telemetry reached the exact team with Alice's single user token.
		d := ingest.await(t, want.marker, func(d e2eDelivery) bool {
			return strings.Contains(d.body, "claude_code.user_prompt")
		})
		if d.token != aliceToken || d.project != want.project {
			t.Fatalf("%s telemetry: token=%q project=%q, want %q / %q", want.session, d.token, d.project, aliceToken, want.project)
		}
		// Verify every delivery for this session (both hook and telemetry) went ONLY to want.project.
		deliveries := ingest.matches(want.session)
		if len(deliveries) < 2 {
			t.Fatalf("%s deliveries = %d, want >= 2 (hook + telemetry)", want.session, len(deliveries))
		}
		for _, del := range deliveries {
			if del.token != aliceToken || del.project != want.project {
				t.Fatalf("%s leaked delivery: token=%q project=%q, want %q / %q", want.session, del.token, del.project, aliceToken, want.project)
			}
		}
	}

	// 2. Org-wide 403 ORG_NOT_ACCESSIBLE on Acme: refuse both Acme enrollments via coalesced
	// refresh nudge while keeping Alice's user token, Beta Labs, and Personal working.
	policiesBefore := f.policies.Load()
	acmeRevoked.Store(true)
	ingest.setRefused(acmeAPI, teamsAcmeWeb)
	runAgentSession(t, bin, acmeRepo, "e2e-acme-revoked", "marker-acme-revoked", addr, relayToken)

	var st routing.State
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var loaded bool
		st, loaded, err = routing.Load(stateDir, config.DefaultProfile)
		if err == nil && loaded {
			now := time.Now()
			acmeRefused := 0
			otherActive := 0
			for _, tm := range st.Teams {
				switch tm.OrganizationID {
				case orgA().ID:
					if tm.StateAt(now) == routing.Refused && tm.LastError == routing.OrgAccessLost {
						acmeRefused++
					}
				default:
					if tm.StateAt(now) == routing.Active {
						otherActive++
					}
				}
			}
			if acmeRefused == 2 && otherActive == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both Acme enrollments to be Refused(OrgAccessLost): %+v (err=%v)", st.Teams, err)
		}
	}
	if ingest.refusedCount(acmeAPI) == 0 {
		t.Fatalf("ingest did not observe a 403 refusal for %s before org-wide Refused transition", acmeAPI)
	}
	if f.policies.Load() <= policiesBefore {
		t.Fatalf("policy refresh did not run after ingest 403 nudge (before=%d after=%d)", policiesBefore, f.policies.Load())
	}
	// Alice's single user token must NOT have been retired or revoked.
	if cred, err := auth.LoadCredential(configDir, config.DefaultProfile); err != nil || cred.AccessToken != aliceToken {
		t.Fatalf("user token after org 403 = %+v, %v; must remain active", cred, err)
	}
	if f.revokes.Load() != 0 {
		t.Fatalf("org 403 revoked user token (%d revokes)", f.revokes.Load())
	}
	if docOut, docErr := runTerma(t, "doctor"); docErr == nil || !strings.Contains(docOut, "no access to this organization, skipped") {
		t.Fatalf("doctor after org 403 (err=%v):\n%s", docErr, docOut)
	}
	if explainOut, err := runTerma(t, "route", "explain", acmeRepo); err != nil || !strings.Contains(explainOut, orgInaccessibleNote) {
		t.Fatalf("route explain after org 403 (%v):\n%s", err, explainOut)
	}
	// Beta Labs and Personal still deliver on Alice's token.
	runAgentSession(t, bin, betaRepo, "e2e-beta-2", "marker-beta-2", addr, relayToken)
	runAgentSession(t, bin, personalRepo, "e2e-personal-2", "marker-personal-2", addr, relayToken)
	if out, err := runTerma(t, "spool", "flush"); err != nil {
		t.Fatalf("spool flush after org 403: %v\n%s", err, out)
	}
	if d := ingest.await(t, "marker-beta-2", nil); d.project != betaCore || d.token != aliceToken {
		t.Fatalf("Beta delivery after Acme 403 = %+v", d)
	}
	if d := ingest.await(t, "marker-personal-2", nil); d.project != teamsPersonalTeam || d.token != aliceToken {
		t.Fatalf("Personal delivery after Acme 403 = %+v", d)
	}

	// Capture Alice's routing CAS expectation before stopping relay and switching accounts.
	aliceAcct := st.Account
	aliceExpect := st.Expect()
	stopRelay()

	// 3. Account switch CAS (alice -> bob): replaces routing.State for bob, revokes alice's
	// session, and rejects stale alice CAS writers with ErrAccountMismatch.
	f.grant("code-bob", bobID)
	bobStdin := identityLine(bobID, orgB().ID) + `{"code":"code-bob"}` + "\n"
	if stdout, stderr, err := (termaRun{within: 30 * time.Second, stdin: strings.NewReader(bobStdin)}).exec(t,
		"setup", "-y", "--desktop-grant", "--insecure-storage", "--relay-service", "off",
		"--org", orgB().ID, "--team", betaCore, "--harness", "claude"); err != nil {
		t.Fatalf("bob setup: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	bobAcct := auth.AccountKey(f.srv.URL, bobID)
	bobSt, ok, err := routing.Load(stateDir, config.DefaultProfile)
	if err != nil || !ok || bobSt.Account != bobAcct || len(bobSt.Teams) != 1 || bobSt.Teams[0].TeamID != betaCore {
		t.Fatalf("bob routing.State = %+v (ok=%v, err=%v)", bobSt, ok, err)
	}
	if revoked := f.revokedTokens(); len(revoked) == 0 {
		t.Fatalf("alice's refresh token was not revoked on account switch")
	}
	if _, err := routing.Update(stateDir, config.DefaultProfile, aliceExpect, func(s *routing.State) error {
		return nil
	}); !errors.Is(err, routing.ErrAccountMismatch) {
		t.Fatalf("stale alice CAS update err = %v, want ErrAccountMismatch", err)
	}
	// Under bob, beta/core routes to Beta Core and acme/api is not collected.
	if out, err := runTerma(t, "route", "explain", betaRepo); err != nil || !strings.Contains(out, "Goes to:   Beta Core") {
		t.Fatalf("bob route explain beta (%v):\n%s", err, out)
	}
	if out, err := runTerma(t, "route", "explain", acmeRepo); err != nil || !strings.Contains(out, "nothing leaves this machine") {
		t.Fatalf("bob route explain acme (%v):\n%s", err, out)
	}
	// Stamping a wrong-account routing.State fails closed in both route explain and doctor.
	if _, err := routing.Replace(stateDir, config.DefaultProfile, nil, aliceAcct, func(s *routing.State) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "route", "explain", "--json", betaRepo); err != nil || !strings.Contains(out, `"state_reason": "wrong_account"`) {
		t.Fatalf("wrong-account route explain (%v):\n%s", err, out)
	}
	if docOut, docErr := runTerma(t, "doctor"); docErr == nil || !strings.Contains(docOut, "belongs to another account") {
		t.Fatalf("wrong-account doctor (err=%v):\n%s", docErr, docOut)
	}
	// Verify spool has no leftover un-flushed alice records leaked to bob.
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if queued, err := sp.Peek(10); err != nil || slices.ContainsFunc(queued, func(e spool.Event) bool { return e.SessionID == "e2e-acme-1" }) {
		t.Fatalf("unexpected alice session events left in spool: %+v (err=%v)", queued, err)
	}
}
