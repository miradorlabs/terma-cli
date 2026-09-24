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

func TestSetupRecordsCodexDesktopSeparatelyFromCLI(t *testing.T) {
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "setup", "--harness", "codex-desktop")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	cfg, err := loadConfig()
	if err != nil || !slices.Equal(cfg.Harnesses, []string{codexDesktopAgent}) {
		t.Fatalf("saved agent choices = %v, %v", cfg.Harnesses, err)
	}
	if !strings.Contains(out, "Codex Desktop") || strings.Contains(out, "Agents recorded: Codex.") {
		t.Fatalf("setup did not name the separate desktop choice:\n%s", out)
	}
}

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range adapter.Names() {
		chosen[name] = true
	}
	form := harnessSelectionForm(context.Background(), chosen)
	wantOrder := []string{"claude", "codex", codexDesktopAgent, "cursor", "opencode", "antigravity"}
	if len(form.Items) != len(wantOrder) {
		t.Fatalf("picker has %d items, want %d", len(form.Items), len(wantOrder))
	}
	for i, name := range wantOrder {
		display := name
		if name == codexDesktopAgent {
			display = "Codex Desktop"
		} else if name == "codex" {
			display = "Codex CLI"
		} else if a, ok := adapter.Lookup(name); ok {
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
	for _, name := range []string{"cursor", "opencode", "antigravity"} {
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
	got, err = parseAgentList("codex-desktop,codex,claude,codex-desktop")
	if err != nil || !slices.Equal(got, []string{"claude", "codex", codexDesktopAgent}) {
		t.Fatalf("independent desktop selection = %v, %v", got, err)
	}
}

func TestHarnessSelectionFiltersSavedAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := &cobra.Command{}
	for _, saved := range [][]string{adapter.Names(), {"cursor", "opencode", "antigravity"}} {
		cfg := &config.Config{Harnesses: saved}
		wantInstalled := []string(nil)
		if slices.Contains(saved, "claude") {
			wantInstalled = []string{"claude", "codex"}
		}
		wantSetup := slices.Clone(wantInstalled)
		if codexDesktopInstalled(context.Background()) {
			wantSetup = append(wantSetup, codexDesktopAgent)
			if len(wantInstalled) == 0 {
				wantInstalled = append(wantInstalled, codexDesktopAgent)
			}
		}
		got, err := chooseHarnesses(cmd, cfg, setupFlags{assumeYes: true})
		if err != nil || !slices.Equal(got, wantSetup) {
			t.Fatalf("setup selection = %v, %v; want %v", got, err, wantSetup)
		}
		got, err = resolveInstallHarnesses(cmd, cfg, installFlags{assumeYes: true, dryRun: true})
		if err != nil || !slices.Equal(got, wantInstalled) {
			t.Fatalf("install selection = %v, %v; want %v", got, err, wantInstalled)
		}
	}
}
