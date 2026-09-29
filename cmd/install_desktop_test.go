package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func desktopInstallSandbox(t *testing.T) (string, *fakeAuth) {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	gitRepoHere(t)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	return home, f
}

// Codex Desktop reads Codex's own global configuration, so choosing it configures that
// file — no PATH shim, no shell startup file — and wires the Codex hooks that announce
// its sessions.
func TestInstallConfiguresCodexDesktopGlobally(t *testing.T) {
	home, gateway := desktopInstallSandbox(t)
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
		p.Harnesses = []string{codexDesktopAgent}
	}); err != nil {
		t.Fatal(err)
	}
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	out, err := within(20*time.Second).combined(t, "install", "--project", projectID, "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install desktop: %v\n%s", err, out)
	}
	if gateway.keysMint.Load() != 1 || keystore.GetFor(shim.AgentCodex, projectID) == "" {
		t.Fatalf("Codex's key missing or minted twice (mints=%d):\n%s", gateway.keysMint.Load(), out)
	}
	if st, err := (harness.Codex{}).Status(); err != nil || !st.Connected || len(st.Signals) == 0 {
		t.Fatalf("Codex's global configuration: %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("install edited the shell startup file: %v", err)
	}
	if len(codexHooksIn(t, mustGetwd(t))["SessionStart"]) != 1 {
		t.Fatal("desktop install did not wire the Codex SessionStart hook")
	}
	if !strings.Contains(out, "Settings → Hooks, then select Review") || !strings.Contains(out, "approve each Terma hook command") {
		t.Fatalf("install did not explain Desktop hook approval:\n%s", out)
	}
}

// Codex CLI and Codex Desktop share one configuration file and one key.
func TestInstallCodexCLIAndDesktopShareOneProjectKey(t *testing.T) {
	_, gateway := desktopInstallSandbox(t)
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	out, err := within(20*time.Second).combined(t, "install", "--harness", "codex,codex-desktop", "--project", projectID, "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install both Codex surfaces: %v\n%s", err, out)
	}
	if gateway.keysMint.Load() != 1 {
		t.Fatalf("Codex CLI and desktop minted %d keys, want one", gateway.keysMint.Load())
	}
}

// A dry run configures nothing machine-wide either.
func TestDesktopInstallDryRunLeavesSettingsUntouched(t *testing.T) {
	home, _ := desktopInstallSandbox(t)
	out, err := runTerma(t, "install", "--harness", "codex-desktop", "--project", "aaaaaaaa-0000-4000-8000-000000000001", "--dry-run")
	if err != nil || !strings.Contains(out, "configures Codex machine-wide") {
		t.Fatalf("desktop dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote Codex's configuration: %v", err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
