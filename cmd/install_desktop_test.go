package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
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

// Codex Desktop is coming soon: a profile an earlier setup saved it in routes the Codex
// CLI alone, and never sets up a Desktop route or says to approve its hooks.
func TestInstallIgnoresASavedCodexDesktopChoice(t *testing.T) {
	_, gateway := desktopInstallSandbox(t)
	if err := config.UpdateProfile(config.DefaultProfile, func(p *config.Profile) {
		p.Harnesses = []string{"codex", codexDesktopAgent}
	}); err != nil {
		t.Fatal(err)
	}
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	out, err := within(20*time.Second).combined(t, "install", "--project", projectID, "--no-path", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if gateway.keysMint.Load() != 1 || keystore.GetFor(shim.AgentCodex, projectID) == "" {
		t.Fatalf("Codex CLI route/key missing or minted twice (mints=%d):\n%s", gateway.keysMint.Load(), out)
	}
	record, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok || !record.CLI || record.Desktop {
		t.Fatalf("route choices = %+v, exists=%v, err=%v; want the CLI alone", record, ok, err)
	}
	if strings.Contains(out, "Codex Desktop") {
		t.Fatalf("install still set up Codex Desktop:\n%s", out)
	}
}

func TestInstallRefusesCodexDesktop(t *testing.T) {
	home, _ := desktopInstallSandbox(t)
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	for _, extra := range [][]string{nil, {"--dry-run"}} {
		args := append([]string{"install", "--harness", "codex-desktop", "--project", projectID, "--yes", "--no-doctor"}, extra...)
		out, err := runTerma(t, args...)
		if err == nil || !strings.Contains(err.Error(), "Coming Soon") {
			t.Fatalf("install %v = %v, want Coming Soon\n%s", extra, err, out)
		}
		if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("a refused install wrote Codex config: %v", err)
		}
	}
}
