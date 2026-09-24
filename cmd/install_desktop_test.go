package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestInstallUsesSavedCodexDesktopChoiceWithoutShellShim(t *testing.T) {
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
		t.Fatalf("desktop route/key missing or minted twice (mints=%d):\n%s", gateway.keysMint.Load(), out)
	}
	record, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok || record.CLI == nil || *record.CLI || record.Desktop == nil || !*record.Desktop {
		t.Fatalf("desktop-only route choices = %+v, exists=%v, err=%v", record, ok, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("desktop-only install edited the shell startup file: %v", err)
	}
	shimDir, err := shim.ShimBinDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "codex")); !os.IsNotExist(err) {
		t.Fatalf("desktop-only install added a Codex CLI PATH shim: %v", err)
	}
	if len(codexHooksIn(t, mustGetwd(t))["SessionStart"]) != 1 {
		t.Fatal("desktop-only install did not wire the Codex SessionStart hook")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("desktop-only install configured a global Codex exporter: %v", err)
	}
	if !strings.Contains(out, "Settings → Hooks, then select Review") || !strings.Contains(out, "approve each Terma hook command") || !strings.Contains(out, "Codex CLI is not required") {
		t.Fatalf("install did not explain Desktop-only hook approval:\n%s", out)
	}
}

func TestDesktopInstallRemovesPreviousRelay(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("legacy LaunchAgent is macOS only")
	}
	home, _ := desktopInstallSandbox(t)
	if err := (harness.Codex{}).Connect(harness.Exporter{
		Endpoint: legacyDesktopBaseURL, Signals: []harness.Signal{harness.SignalLogs},
	}, false); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(home, "Library", "LaunchAgents", desktopServiceLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(service), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	if out, err := within(20*time.Second).combined(t, "install", "--harness", "codex-desktop",
		"--project", projectID, "--yes", "--no-doctor"); err != nil {
		t.Fatalf("migrate install: %v\n%s", err, out)
	}
	status, err := (harness.Codex{}).Status()
	if err != nil || status.Connected {
		t.Fatalf("old exporter still configured: %+v, %v", status, err)
	}
	if _, err := os.Stat(service); !os.IsNotExist(err) {
		t.Fatalf("old LaunchAgent remains: %v", err)
	}
}

func TestInstallCodexCLIAndDesktopShareOneProjectKey(t *testing.T) {
	_, gateway := desktopInstallSandbox(t)
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	out, err := within(20*time.Second).combined(t, "install", "--harness", "codex,codex-desktop", "--project", projectID,
		"--no-path", "--yes", "--no-doctor")
	if err != nil {
		t.Fatalf("install both Codex surfaces: %v\n%s", err, out)
	}
	if gateway.keysMint.Load() != 1 {
		t.Fatalf("Codex CLI and desktop minted %d keys, want one", gateway.keysMint.Load())
	}
	shimDir, err := shim.ShimBinDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shimDir, "codex")); err != nil {
		t.Fatalf("CLI choice did not install its PATH shim: %v", err)
	}
	record, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok || record.CLI == nil || !*record.CLI || record.Desktop == nil || !*record.Desktop {
		t.Fatalf("combined route choices = %+v, exists=%v, err=%v", record, ok, err)
	}
}

func TestDesktopInstallDryRunAndMissingLogsLeaveSettingsUntouched(t *testing.T) {
	home, _ := desktopInstallSandbox(t)
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	for _, args := range [][]string{
		{"install", "--harness", "codex-desktop", "--project", projectID, "--dry-run"},
		{"install", "--harness", "codex-desktop", "--project", projectID, "--signals", "traces"},
	} {
		out, err := runTerma(t, args...)
		if slices.Contains(args, "--dry-run") {
			if err != nil || !strings.Contains(out, "Settings → Hooks → Review in Codex Desktop") {
				t.Fatalf("desktop dry run: %v\n%s", err, out)
			}
		} else if err == nil || !strings.Contains(err.Error(), "logs signal") {
			t.Fatalf("desktop without logs: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
			t.Fatalf("desktop config written before install applied: %v", err)
		}
	}
}

func TestDesktopInstallRequiresCodexRepositoryHooks(t *testing.T) {
	home, _ := desktopInstallSandbox(t)
	const projectID = "aaaaaaaa-0000-4000-8000-000000000001"
	for _, extra := range [][]string{{"--no-hooks"}, {"--adapters", "claude"}} {
		args := append([]string{"install", "--harness", "codex-desktop", "--project", projectID, "--yes", "--no-doctor"}, extra...)
		out, err := runTerma(t, args...)
		if err == nil || !strings.Contains(err.Error(), "Codex repository hooks") && !strings.Contains(err.Error(), "SessionStart repository hook") {
			t.Fatalf("missing Codex hooks accepted: %v\n%s", err, out)
		}
		if _, statErr := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(statErr) {
			t.Fatalf("desktop exporter installed without its session hook: %v", statErr)
		}
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
