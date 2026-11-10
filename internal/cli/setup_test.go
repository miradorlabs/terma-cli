package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// After teardown removes the policies, setup without a terminal reuses the team the profile
// selected, even in an organization with several teams.
func TestSetupAfterTeardownReusesTheSelectedTeam(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	t.Setenv("TERMA_POLICY_STUB", "")
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	selected := projectsIn(orgA().ID)[1]
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OrganizationID, p.Team = orgA().ID, selected.ID }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetContext(t.Context())
	if name, err := testApp.selectPolicyTeam(cmd, cfg, false); err != nil || name != selected.Name || cfg.ProjectID != selected.ID {
		t.Fatalf("selectPolicyTeam = %q, %v; project %q, want %s", name, err, cfg.ProjectID, selected.ID)
	}
}

// On a terminal, the team an earlier setup saved is only the picker's default in an
// organization with several, where a team created since is easily the one meant; --team
// and an organization with one team are taken without asking, and a saved team since
// deleted gives way to the only one, or, when setup cannot ask, is refused by name.
func TestSetupOffersTheSavedTeamAsTheDefault(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	t.Setenv("TERMA_POLICY_STUB", "")
	for _, c := range []struct {
		name     string
		org      organization
		explicit bool
		gone     bool
		ask      bool
		asks     bool
	}{
		{"several teams, saved", orgA(), false, false, true, true},
		{"several teams, --team", orgA(), true, false, true, false},
		{"one team, saved", orgB(), false, false, true, false},
		{"one team, saved team deleted", orgB(), false, true, false, false},
		{"several teams, saved team deleted, cannot ask", orgA(), false, true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, c.org)); err != nil {
				t.Fatal(err)
			}
			saved := projectsIn(c.org.ID)[len(projectsIn(c.org.ID))-1]
			savedID := saved.ID
			if c.gone {
				savedID = "dddddddd-0000-4000-8000-000000000009"
			}
			if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.OrganizationID, p.Team = c.org.ID, savedID }); err != nil {
				t.Fatal(err)
			}
			cfg, err := testApp.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if c.explicit {
				cfg.ProjectID = saved.ID
			}
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(""))
			cmd.SetContext(t.Context())
			// No terminal here, so a picker fails: what matters is whether setup asked.
			name, err := testApp.selectPolicyTeam(cmd, cfg, c.ask)
			if asked := err != nil && strings.Contains(err.Error(), "no terminal to prompt on"); asked != c.asks {
				t.Fatalf("selectPolicyTeam = %q, %v; asked %v, want %v", name, err, asked, c.asks)
			}
			if c.gone && len(projectsIn(c.org.ID)) > 1 {
				if err == nil || !strings.Contains(err.Error(), "no longer in this organization") {
					t.Fatalf("selectPolicyTeam = %q, %v; want the deleted team refused by name", name, err)
				}
				return
			}
			if !c.asks && (err != nil || name != saved.Name || cfg.ProjectID != saved.ID) {
				t.Fatalf("selectPolicyTeam = %q, %v; project %q, want %s", name, err, cfg.ProjectID, saved.ID)
			}
		})
	}
}

// setup records the collection policy and points the agents at the relay, no repository involved.
func TestSetupFetchesThePolicyAndPointsAgentsAtTheRelay(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	loaded, err := testApp.loadConfig()
	if p := loaded.Policy; err != nil || !p.Validated() || p.Mode != config.ModeRepo || !p.IncludePrompts || p.FetchedAt.IsZero() {
		t.Fatalf("policy not recorded: %+v", p)
	}
	if !strings.Contains(out, "Collects      sessions in the team's repositories (github.com/acme/app)") {
		t.Fatalf("setup did not say the policy:\n%s", out)
	}
	// The test binary is no release, so it is never updated; setup says so.
	if !strings.Contains(out, "Updates       never for a development build") {
		t.Fatalf("setup did not say how terma updates:\n%s", out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatalf("no relay token after setup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil || !strings.Contains(string(data), "127.0.0.1:43180") || !strings.Contains(string(data), token) {
		t.Fatalf("Codex not pointed at the relay: %v\n%s", err, data)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex", "--relay-service", "sometimes"); err == nil {
		t.Fatalf("--relay-service sometimes was accepted:\n%s", out)
	}
}

// A team that lists no repositories collects nothing, and setup says so as a step left to do.
func TestSetupWarnsWhenTheTeamListsNoRepositories(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"repo","repositories":[],"include_prompts":true,"include_tool_content":true,"default_project_id":"team"}`)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"! Collects      nothing yet: your team lists no repositories", "Next steps:", doctor.NoRepositoriesStep} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ Setup complete") {
		t.Errorf("setup called itself complete while collecting nothing:\n%s", out)
	}
}

// A team no admin has given a collection policy collects nothing; setup and doctor say the
// policy does not exist yet and who sets it up, not that the team lists no repositories.
func TestSetupAndDoctorSayWhenTheTeamHasNoPolicy(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.policyBody = `{}`
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TERMA_POLICY_STUB", "")
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgB())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"! Collects      nothing yet: your team has no collection policy", "Next steps:", doctor.NoPolicyStep} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "lists no repositories") || strings.Contains(out, doctor.NoRepositoriesStep) {
		t.Errorf("setup blamed an empty repository list:\n%s", out)
	}
	// No policy has git_hooks off too, but the step is the policy, not commit hooks.
	if strings.Contains(out, doctor.GitHooksOffStep) {
		t.Errorf("setup recommended commit hooks to a team with no policy:\n%s", out)
	}
	loaded, err := testApp.loadConfig()
	if err != nil || !loaded.Policy.Unset || !loaded.Policy.AdmitsNone() {
		t.Fatalf("stored policy = %+v, %v; want unset and admitting nothing", loaded.Policy, err)
	}
	// Doctor names the team and organization it asked, to hold against the web app's.
	out, _ = runTerma(t, "doctor")
	if !regexp.MustCompile(`repository collected +team "Beta Core" \(bbbbbbbb\) in Beta Labs has no collection policy\n`).MatchString(out) || !strings.Contains(out, doctor.NoPolicyStep) {
		t.Errorf("doctor did not say which team has no policy:\n%s", out)
	}
}

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range testApp.agents.Names() {
		chosen[name] = true
	}
	form := testApp.harnessSelectionForm(context.Background(), chosen)
	wantOrder := []string{"claude", "codex", "copilot", "cursor", "opencode", "omp", "pi", "hermes", "gemini", "dsh", "antigravity"}
	if len(form.Items) != len(wantOrder) {
		t.Fatalf("picker has %d items, want %d", len(form.Items), len(wantOrder))
	}
	for i, name := range wantOrder {
		display := name
		if s, _, ok := testApp.agents.Surface(name); ok {
			display = s.DisplayName
		}
		if form.Items[i].Label != display {
			t.Errorf("picker row %d = %q, want %q", i, form.Items[i].Label, display)
		}
		item := form.Items[i]
		available := testApp.agents.IsSupported(name)
		if available {
			if item.Disabled || item.Selected != chosen[name] {
				t.Errorf("%s should be selectable with its saved choice: %+v", name, item)
			}
		} else if !item.Disabled || item.Selected || item.Reason != "Coming Soon" {
			t.Errorf("%s should be disabled, unselected, and labeled Coming Soon: %+v", name, item)
		}
	}
}

func TestHarnessSelectionFlags(t *testing.T) {
	for _, name := range []string{"cursor", "opencode", "antigravity"} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cfg := &config.Config{}
			if _, err := testApp.chooseHarnesses(cmd, cfg, setupFlags{harnesses: "claude," + name}); err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("setup error = %v", err)
			}
		})
	}
	got, err := testApp.parseAgentList("codex,claude,codex")
	if err != nil || !slices.Equal(got, []string{"claude", "codex"}) {
		t.Fatalf("available selection = %v, %v", got, err)
	}
}

func TestHarnessSelectionFiltersSavedAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := &cobra.Command{}
	for _, saved := range [][]string{testApp.agents.Names(), {"cursor", "opencode", "antigravity"}} {
		cfg := &config.Config{Harnesses: saved}
		wantInstalled := []string(nil)
		if slices.Contains(saved, "claude") {
			wantInstalled = []string{"claude", "codex", "copilot"}
		}
		if len(wantInstalled) == 0 {
			// With no CLI on PATH, an installed desktop app or editor still counts.
			for _, n := range []string{"claude", "codex", "copilot"} {
				if s, _, _ := testApp.agents.Surface(n); s.Installed(context.Background()) {
					wantInstalled = append(wantInstalled, n)
				}
			}
		}
		wantSetup := slices.Clone(wantInstalled)
		got, err := testApp.chooseHarnesses(cmd, cfg, setupFlags{assumeYes: true})
		if err != nil || !slices.Equal(got, wantSetup) {
			t.Fatalf("setup selection = %v, %v; want %v", got, err, wantSetup)
		}
	}
}

// setup is the developer's own command, so it records their environment as the relay's:
// a relay a hook starts later runs in it even when the hook's shell lacks TERMA_ENV.
func TestSetupRecordsTheRelayEnvironment(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	dir, err := daemon.Dir(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	env, ok := daemon.RecordedEnv(dir)
	if !ok {
		t.Fatal("setup recorded no relay environment")
	}
	for _, k := range []string{"TERMA_ENV", "TERMA_CONFIG_DIR", "HOME"} {
		if env[k] != os.Getenv(k) {
			t.Errorf("recorded %s=%q, setup ran with %q", k, env[k], os.Getenv(k))
		}
	}
}

// Setup leaves an agent it was not asked for and never configured alone, even when its config does not parse.
func TestSetupLeavesAnUnconfiguredAgentsConfigAlone(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	mine := "model = \"gpt-6\"\n[otel\n"
	if err := os.WriteFile(codexConfig, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "claude"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")); !strings.Contains(string(data), token) {
		t.Fatalf("Claude Code is not on the relay:\n%s", data)
	}
	if data, _ := os.ReadFile(codexConfig); string(data) != mine {
		t.Fatalf("Codex's config changed:\n%s", data)
	}
}

// An agent dropped at a later setup stops sending to the relay, while the agent still chosen keeps it.
func TestSetupReleasesTheRelayFromADeselectedAgent(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(codexHome, "config.toml")
	mine := "model = \"gpt-6\"\n\n[otel]\nexporter = { otlp-http = { endpoint = \"https://otel.example.com/v1/logs\", protocol = \"binary\" } }\n\n[otel.tool_result]\nmax_bytes = 4096\n"
	if err := os.WriteFile(codexConfig, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	// An agent terma never configured, whose own settings terma cannot parse, is no obstacle.
	t.Setenv("GEMINI_CLI_HOME", t.TempDir())
	geminiSettings := filepath.Join(os.Getenv("GEMINI_CLI_HOME"), ".gemini", "settings.json")
	geminiMine := "// mine\n{}\n"
	if err := os.MkdirAll(filepath.Dir(geminiSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(geminiSettings, []byte(geminiMine), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeSettings := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	read := func(path string) string {
		t.Helper()
		data, _ := os.ReadFile(path)
		return string(data)
	}

	if out, err := runTerma(t, "setup", "--harness", "claude,codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	token, err := daemon.Token(testApp.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(codexConfig), token) || !strings.Contains(read(claudeSettings), token) {
		t.Fatalf("setup did not point both agents at the relay:\ncodex:\n%s\nclaude:\n%s", read(codexConfig), read(claudeSettings))
	}

	out, err := runTerma(t, "setup", "--harness", "claude", "--verbose")
	if err != nil {
		t.Fatalf("setup without Codex: %v\n%s", err, out)
	}
	if got := read(codexConfig); strings.Contains(got, token) || !strings.Contains(got, "otel.example.com") || !strings.Contains(got, "max_bytes = 4096") {
		t.Fatalf("Codex's own [otel] is not back:\n%s", got)
	}
	if !strings.Contains(out, "Codex") || !strings.Contains(out, "no longer sends to the local relay") {
		t.Errorf("setup did not say Codex stopped sending:\n%s", out)
	}
	if !strings.Contains(read(claudeSettings), token) {
		t.Fatalf("Claude Code, still chosen, left the relay:\n%s", read(claudeSettings))
	}

	// And the other way round: dropping Claude Code takes its env block off the relay.
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup without Claude Code: %v\n%s", err, out)
	}
	if got := read(claudeSettings); strings.Contains(got, token) {
		t.Fatalf("Claude Code still sends to the relay:\n%s", got)
	}
	if !strings.Contains(read(codexConfig), token) {
		t.Fatalf("Codex, chosen again, is not on the relay:\n%s", read(codexConfig))
	}

	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
	if got := read(codexConfig); strings.Contains(got, token) {
		t.Fatalf("Codex still sends to the relay after teardown:\n%s", got)
	}
	if got := read(geminiSettings); got != geminiMine {
		t.Fatalf("Gemini CLI's own settings changed:\n%s", got)
	}
}

// otlpRequest is one request an ingest host received: the key it came with and its body.
type otlpRequest struct {
	auth string
	body []byte
}

// recordingIngest is an ingest host that takes every request and keeps it.
type recordingIngest struct {
	*httptest.Server
	mu       sync.Mutex
	requests []otlpRequest
}

func newRecordingIngest(t *testing.T) *recordingIngest {
	t.Helper()
	h := &recordingIngest{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, otlpRequest{auth: r.Header.Get("Authorization"), body: body})
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.Close)
	return h
}

// take returns the requests received since the last take.
func (h *recordingIngest) take() []otlpRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	got := h.requests
	h.requests = nil
	return got
}

// The cutover when Terma Desktop moves the CLI to another organization (the organization-
// model council's item 1a), proved on the existing routing: work in flight keeps its team
// and that team's restrictions, nothing is sent for the old team while the profile is on
// the new organization, never with the new team's key nor under its wider policy, and a
// switch back collects for the old team under its own policy again.
func TestDesktopGrantCutoverKeepsInFlightWorkWithItsTeam(t *testing.T) {
	gateway := grantSandbox(t)
	t.Setenv("TERMA_POLICY_STUB", "") // the real fetches, which the account service scopes
	gateway.scopedPolicies = true
	teamA, personal := teamOrgTeam(), personalTeam()
	gateway.policyBodies = map[string]string{
		// The SHARED organization's team: one repository, no prompts, no tool content.
		teamA: `{"policy":{"version":"1.1","terma":{"per_repository":{"repositories":["github.com/acme/app"]},"git_hooks":false,"capture":{"exclude_prompts":true,"exclude_tool_content":true}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
		// Personal's starts on Collect All: every session, all content.
		personal: `{"policy":{"version":"1.1","terma":{"global":{},"git_hooks":false,"capture":{"exclude_prompts":false,"exclude_tool_content":false}}},"revision":1,"updated_at":"2026-11-01T00:00:00Z"}`,
	}
	ingest := newRecordingIngest(t)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)
	gateway.grant("to-team", orgA().ID, deskUser)
	gateway.grant("to-personal", personalOrg().ID, deskUser)
	setupFor := func(org organization, team, code string) {
		t.Helper()
		stdin := identityLine(deskUser, org.ID)
		if code != "" {
			stdin += `{"code":"` + code + `"}` + "\n"
		}
		if stdout, stderr, err := desktopSetup(t, stdin, org.ID, team); err != nil {
			t.Fatalf("setup for %s: %v\n%s%s", org.Name, err, stdout, stderr)
		}
		ingest.take() // setup's own check-in is not the delivery under test
	}
	keyOf := func(team string) string {
		t.Helper()
		key, err := keystore.Get(testApp.dir, team)
		if err != nil || key == "" {
			t.Fatalf("no key for team %s: %v", team, err)
		}
		return key
	}
	// resolve is what a relay started now does with the claim of session sid.
	resolve := func(sid string) (relay.Policy, error) {
		t.Helper()
		c, ok := claim.Read(testApp.stateDir, sid, time.Now())
		if !ok {
			t.Fatalf("session %s has no claim", sid)
		}
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		return testApp.relayDeps().Resolver(cfg, nil)(c)
	}
	flush := func() []otlpRequest {
		t.Helper()
		if _, err := testApp.flushSpool(t.Context(), true, 0); err != nil {
			t.Fatal(err)
		}
		return ingest.take()
	}

	// On the SHARED organization: events queued for team A, and a session claimed for it.
	setupFor(orgA(), teamA, "to-team")
	keyA := keyOf(teamA)
	s := spoolForTest(t)
	appendEvent(t, s, teamA, time.Now())
	appendEvent(t, s, teamA, time.Now())
	if !claim.Write(testApp.stateDir, "in-flight", claim.Claim{ProjectID: teamA, Tool: "codex", Repository: appRepo}, time.Now()) {
		t.Fatal("could not claim the session")
	}
	if pol, err := resolve("in-flight"); err != nil || pol.Key != keyA || pol.IncludePrompts || pol.IncludeToolContent {
		t.Fatalf("before the switch the claim resolves to %+v, %v; want team A's key and policy", pol, err)
	}

	// Onboarding moves the CLI to Personal.
	setupFor(personalOrg(), personal, "to-personal")
	keyP := keyOf(personal)
	appendEvent(t, s, personal, time.Now()) // Personal's own, which must still go
	sent := flush()
	if len(sent) == 0 {
		t.Fatal("Personal's own event was not delivered: the pass proves nothing")
	}
	for _, r := range sent {
		if r.auth != "Bearer "+keyP || bytes.Contains(r.body, []byte(teamA)) {
			t.Fatalf("while on Personal a request went with %q carrying team A: %v", r.auth, bytes.Contains(r.body, []byte(teamA)))
		}
	}
	if queued := queuedProjects(t); len(queued) != 2 || queued[0] != teamA || queued[1] != teamA {
		t.Fatalf("queue = %v, want team A's two events kept under team A", queued)
	}
	// A relay started on Personal neither sends the claimed session with Personal's key nor
	// under Personal's policy: it has no validated policy for team A, so it withholds.
	if pol, err := resolve("in-flight"); err == nil && (pol.Key == keyP || pol.Signals == nil || len(pol.Signals) != 0) {
		t.Fatalf("on Personal the claim for team A resolves to %+v", pol)
	}

	// The desktop switches back: the stored sign-in serves, and team A collects under its own
	// policy, with its own key.
	setupFor(orgA(), teamA, "")
	if gateway.exchanges.Load() != 2 {
		t.Fatalf("the switch back exchanged a code (%d exchanges); it reuses the stored sign-in", gateway.exchanges.Load())
	}
	pol, err := resolve("in-flight")
	if err != nil || pol.Key != keyA || pol.IncludePrompts || pol.IncludeToolContent || pol.Unadmitted || pol.Signals != nil {
		t.Fatalf("back on the SHARED organization the claim resolves to %+v, %v; want team A's key and restrictions", pol, err)
	}
	appendEvent(t, s, teamA, time.Now())
	sent = flush()
	if len(sent) == 0 {
		t.Fatal("team A's events were not delivered after the switch back")
	}
	for _, r := range sent {
		if r.auth != "Bearer "+keyA {
			t.Fatalf("team A's events went with %q, not team A's own key", r.auth)
		}
	}
	if queued := queuedProjects(t); len(queued) != 0 {
		t.Fatalf("queue = %v after the switch back", queued)
	}
}
