package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/spf13/cobra"
)

// The desktop apps run their own copy of their agent, so the shim that routes the CLI
// never reaches them: setup offers them as coming soon, and names Claude as the CLI.
func TestSetupNamesClaudeCodeAndRefusesTheDesktopApps(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "claude")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Agents recorded: Claude Code.") {
		t.Fatalf("setup should name Claude Code alone:\n%s", out)
	}
	for _, name := range []string{codexDesktopAgent, claudeDesktopAgent, copilotAgent} {
		out, err := runTerma(t, "setup", "--harness", name)
		if err == nil || !strings.Contains(err.Error(), "Coming Soon") {
			t.Fatalf("setup --harness %s = %v, want Coming Soon\n%s", name, err, out)
		}
	}
}

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range adapter.Names() {
		chosen[name] = true
	}
	form := harnessSelectionForm(context.Background(), chosen)
	wantOrder := []string{"claude", "codex", claudeDesktopAgent, codexDesktopAgent, "cursor", "opencode", "antigravity", copilotAgent}
	if len(form.Items) != len(wantOrder) {
		t.Fatalf("picker has %d items, want %d", len(form.Items), len(wantOrder))
	}
	for i, name := range wantOrder {
		display := map[string]string{
			"claude": "Claude Code", "codex": "Codex CLI",
			claudeDesktopAgent: "Claude Desktop", codexDesktopAgent: "Codex Desktop", copilotAgent: "GitHub Copilot",
		}[name]
		if a, ok := adapter.Lookup(name); ok && display == "" {
			display = a.DisplayName()
		}
		if form.Items[i].Label != display {
			t.Errorf("picker row %d = %q, want %q", i, form.Items[i].Label, display)
		}
		item := form.Items[i]
		available := agentAvailable(name)
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
	for _, name := range []string{"cursor", "opencode", "antigravity", codexDesktopAgent, claudeDesktopAgent, copilotAgent} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cfg := &config.Config{}
			if _, err := chooseHarnesses(cmd, cfg, setupFlags{harnesses: "claude," + name}); err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("setup error = %v", err)
			}
			if _, err := resolveInstallHarnesses(cmd, cfg, installFlags{harnesses: name}); err == nil || !strings.Contains(err.Error(), "Coming Soon") {
				t.Fatalf("install error = %v", err)
			}
		})
	}
	got, err := parseAgentList("codex,claude,codex")
	if err != nil || !slices.Equal(got, []string{"claude", "codex"}) {
		t.Fatalf("available selection = %v, %v", got, err)
	}
	if _, err := parseAgentList("copilot-cli"); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown agent error = %v", err)
	}
}

func TestHarnessSelectionFiltersSavedAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := &cobra.Command{}
	// A profile an earlier setup wrote may hold codex-desktop; it is dropped, never an error.
	for _, saved := range [][]string{append(adapter.Names(), codexDesktopAgent), {"cursor", "opencode", "antigravity", codexDesktopAgent}} {
		cfg := &config.Config{Harnesses: saved}
		want := []string(nil)
		if slices.Contains(saved, "claude") {
			want = []string{"claude", "codex"}
		}
		got, err := chooseHarnesses(cmd, cfg, setupFlags{assumeYes: true})
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("setup selection = %v, %v; want %v", got, err, want)
		}
		got, err = resolveInstallHarnesses(cmd, cfg, installFlags{assumeYes: true, dryRun: true})
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("install selection = %v, %v; want %v", got, err, want)
		}
	}
}
