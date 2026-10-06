package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/spool"
)

func nateTestEnvironment(t *testing.T) (home, configDir, stateDir, workspace string) {
	t.Helper()
	home = t.TempDir()
	configDir = filepath.Join(home, ".config", "terma")
	stateDir = filepath.Join(home, ".local", "state", "terma")
	workspace = t.TempDir()
	t.Chdir(workspace)
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	useDirs(t, configDir, stateDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	return home, configDir, stateDir, workspace
}

func stubNateBinary(t *testing.T, path string) {
	t.Helper()
	oldCandidates, oldRemove := testApp.nateBinaryCandidates, testApp.nateRemoveBinary
	testApp.nateBinaryCandidates = func() []string { return []string{path} }
	testApp.nateRemoveBinary = removeNateBinary
	t.Cleanup(func() {
		testApp.nateBinaryCandidates, testApp.nateRemoveBinary = oldCandidates, oldRemove
	})
}

func TestNateRequiresExplicitConfirmationWithoutATerminal(t *testing.T) {
	_, configDir, _, _ := nateTestEnvironment(t)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(configDir, "keep-until-confirmed")
	if err := os.WriteFile(marker, []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "nate")
	if err == nil || !strings.Contains(err.Error(), "Are you sure you want to do this? It will remove everything related to Terma on this machine.") {
		t.Fatalf("nate without confirmation = %v, output %q", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("nate changed state before confirmation: %v", err)
	}
}

func TestNateRemovesMachineStateAndBinaryButNotTheRepository(t *testing.T) {
	home, configDir, stateDir, workspace := nateTestEnvironment(t)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "credentials.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, spool.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".terma"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".terma", "settings.json"), []byte(`{"version":1,"project":{"id":"p"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "bin", "terma")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubNateBinary(t, binary)

	out, err := runTerma(t, "nate", "--yes")
	if err != nil {
		t.Fatalf("nate: %v\n%s", err, out)
	}
	for _, path := range []string{configDir, stateDir, binary} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived nate: %v", path, err)
		}
	}
	// nate must not touch the repository it ran in.
	if _, err := os.Stat(filepath.Join(workspace, ".terma", "settings.json")); err != nil {
		t.Errorf("nate removed a file in the repository: %v", err)
	}
	if !strings.Contains(out, "Terma has been removed") {
		t.Fatalf("missing completion message:\n%s", out)
	}
}

func TestNateKeepsRecoveryStateWhenRestorationFails(t *testing.T) {
	home, configDir, _, _ := nateTestEnvironment(t)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(configDir, "journal")
	if err := os.WriteFile(marker, []byte("needed to recover"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeSettings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudeSettings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeSettings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "bin", "terma")
	stubNateBinary(t, binary)

	out, err := runTerma(t, "nate", "--yes")
	if err == nil || !strings.Contains(err.Error(), "restore Claude Code") {
		t.Fatalf("nate with unreadable managed config = %v, output %q", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("recovery state was deleted after restoration failed: %v", err)
	}
}

func TestRemoveTermaDirRefusesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := removeTermaDir(home); err == nil {
		t.Fatal("home directory was accepted as a Terma directory")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("home directory was removed: %v", err)
	}
}
