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
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// setup records the collection policy and points the agents at the relay, no repository involved.
func TestSetupFetchesThePolicyAndPointsAgentsAtTheRelay(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	file, err := config.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	p := file.Profiles[config.DefaultProfile]
	if p == nil || p.Policy == nil || p.Policy.Mode != config.ModeRepo || !p.Policy.IncludePrompts || p.Policy.FetchedAt.IsZero() {
		t.Fatalf("policy not recorded: %+v", p)
	}
	if !strings.Contains(out, "Collects      sessions in connected repositories") {
		t.Fatalf("setup did not say the policy:\n%s", out)
	}
	// Install approves Codex's hooks itself, so setup asks for no review in Codex.
	if strings.Contains(out, "`/hooks`") {
		t.Fatalf("setup still asks for a /hooks review that install does itself:\n%s", out)
	}
	token, err := daemon.Token()
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
			if _, _, err := testApp.resolveInstallHarnesses(cmd, cfg, installFlags{harnesses: name}); err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("install error = %v", err)
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
		got, _, err = testApp.resolveInstallHarnesses(cmd, cfg, installFlags{assumeYes: true, dryRun: true})
		if err != nil || !slices.Equal(got, wantInstalled) {
			t.Fatalf("install selection = %v, %v; want %v", got, err, wantInstalled)
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
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "setup", "--harness", "codex"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	dir, err := daemon.Dir()
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
