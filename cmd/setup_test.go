package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/spf13/cobra"
)

func TestHarnessSelectionComingSoon(t *testing.T) {
	chosen := map[string]bool{}
	for _, name := range adapter.Names() {
		chosen[name] = true
	}
	form := harnessSelectionForm(context.Background(), chosen)
	wantOrder := []string{"claude", "codex", "cursor", "opencode", "antigravity"}
	if len(form.Items) != len(wantOrder) {
		t.Fatalf("picker has %d items, want %d", len(form.Items), len(wantOrder))
	}
	for i, name := range wantOrder {
		a, _ := adapter.Lookup(name)
		if form.Items[i].Label != a.DisplayName() {
			t.Errorf("picker row %d = %q, want %q", i, form.Items[i].Label, a.DisplayName())
		}
		item := form.Items[i]
		available := a.Name() == "claude" || a.Name() == "codex"
		if available {
			if item.Disabled || !item.Selected {
				t.Errorf("%s should be selectable and preselected: %+v", a.Name(), item)
			}
		} else if !item.Disabled || item.Selected || item.Reason != "Coming Soon" {
			t.Errorf("%s should be disabled, unselected, and labeled Coming Soon: %+v", a.Name(), item)
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
}

func TestHarnessSelectionFiltersSavedAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := &cobra.Command{}
	for _, saved := range [][]string{adapter.Names(), {"cursor", "opencode", "antigravity"}} {
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
