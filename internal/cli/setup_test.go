package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
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
	if name, err := testApp.selectPolicyTeam(cmd, cfg); err != nil || name != selected.Name || cfg.ProjectID != selected.ID {
		t.Fatalf("selectPolicyTeam = %q, %v; project %q, want %s", name, err, cfg.ProjectID, selected.ID)
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

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range testApp.agents.Names() {
		chosen[name] = true
	}
	form := testApp.harnessSelectionForm(context.Background(), chosen)
	wantOrder := []string{"claude", "codex", "cursor", "opencode", "omp", "pi", "hermes", "gemini", "dsh", "antigravity", "GitHub Copilot"}
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
			wantInstalled = []string{"claude", "codex"}
		}
		if len(wantInstalled) == 0 {
			// With no CLI on PATH, an installed desktop app still counts.
			for _, n := range []string{"claude", "codex"} {
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
