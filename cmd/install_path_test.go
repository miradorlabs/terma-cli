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
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// codexInstall runs `terma install` for Codex against the fake gateway, in a home
// directory of the test's own with zsh as the login shell, and returns the output and
// the startup file an earlier terma may have written.
func codexInstall(t *testing.T, before func(home string), extra ...string) (out, zshrc string) {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	gitRepoHere(t)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(home)
	}
	args := append([]string{"install", "--harness", "codex", "--project", "aaaaaaaa-0000-4000-8000-000000000001", "--no-hooks", "--no-doctor", "--no-browser"}, extra...)
	out, err := within(20*time.Second).combined(t, args...)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	return out, filepath.Join(home, ".zshrc")
}

// install no longer touches the developer's shell: there are no shims to put on PATH,
// and nothing to reload. Codex is configured in its own global file instead.
func TestInstallConfiguresCodexGloballyWithoutTouchingTheShell(t *testing.T) {
	out, zshrc := codexInstall(t, nil, "--yes")
	if _, err := os.Stat(zshrc); !os.IsNotExist(err) {
		t.Fatalf("install wrote the shell's startup file: %v", err)
	}
	if strings.Contains(out, "source ") || strings.Contains(out, "PATH") {
		t.Fatalf("install still talks about the shell:\n%s", out)
	}
	st, err := (harness.Codex{}).Status()
	if err != nil || !st.Connected || len(st.Signals) == 0 {
		t.Fatalf("Codex's global configuration after install: %+v, %v", st, err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Mode != config.TelemetryDirect || cfg.Telemetry.Project.ID != "aaaaaaaa-0000-4000-8000-000000000001" {
		t.Fatalf("the machine's telemetry record: %+v", cfg.Telemetry)
	}
}

// A machine an earlier terma routed per repository has PATH shims, the block in the
// startup file that put them first, and a routing record per project. install takes all
// of it away, keeping every other line of the startup file.
func TestInstallRemovesTheOldPerRepositoryRouting(t *testing.T) {
	var binDir string
	out, zshrc := codexInstall(t, func(home string) {
		binDir, _ = shim.ShimBinDir()
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\n# terma per-repo routing shim for codex.\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export EDITOR=vim\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, ok := shim.ShellRC()
		if !ok {
			t.Fatal("no startup file for zsh")
		}
		if _, err := rc.Ensure(binDir); err != nil {
			t.Fatal(err)
		}
		if err := shim.SaveRecord(shim.Record{ProjectID: "aaaaaaaa-0000-4000-8000-000000000001", Harnesses: []string{"codex"}, CLI: true}); err != nil {
			t.Fatal(err)
		}
	}, "--yes", "--verbose")
	if !strings.Contains(out, "removed the old per-repository routing") {
		t.Fatalf("install did not say it cleaned up:\n%s", out)
	}
	data, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "export EDITOR=vim\n" {
		t.Fatalf("startup file after cleanup:\n%s", data)
	}
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("the shim directory survived: %v", err)
	}
	if _, ok, _ := shim.LoadRecord("aaaaaaaa-0000-4000-8000-000000000001"); ok {
		t.Fatal("the routing record survived")
	}
}

// Flags only per-repository routing had are accepted and ignored, so a script that still
// passes them keeps working.
func TestInstallAcceptsTheRetiredRoutingFlags(t *testing.T) {
	codexInstall(t, nil, "--yes", "--no-path", "--activation", "wrapper")
}

// `source` is not POSIX: a dash or BusyBox ash user is told `.`.
func TestReloadCommandMatchesTheShell(t *testing.T) {
	for shell, want := range map[string]string{
		"/bin/zsh": "source ~/.zshrc", "/usr/bin/fish": "source ~/.zshrc", "/bin/dash": ". ~/.zshrc", "": ". ~/.zshrc",
	} {
		t.Setenv("SHELL", shell)
		if got := reloadCommand("~/.zshrc"); got != want {
			t.Errorf("SHELL=%q: reloadCommand = %q, want %q", shell, got, want)
		}
	}
}
