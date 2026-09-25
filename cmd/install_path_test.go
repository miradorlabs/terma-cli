package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// routedInstall runs `terma install` for a routable agent against the fake gateway, in a
// home directory of the test's own with zsh as the login shell, and returns the output
// and the startup file it may have written.
func routedInstall(t *testing.T, extra ...string) (out, zshrc string) {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv(shim.WrapperEnv, "")
	t.Setenv("PATH", "/usr/bin:/bin") // no shim ahead of anything: routing is not live
	gitRepoHere(t)
	if _, err := auth.SaveCredential(config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"install", "--harness", "codex", "--project", "aaaaaaaa-0000-4000-8000-000000000001", "--no-hooks", "--no-doctor", "--no-browser"}, extra...)
	out, err := within(20*time.Second).combined(t, args...)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	return out, filepath.Join(home, ".zshrc")
}

// The last manual step of per-repo routing: the shim directory has to be on PATH, ahead
// of the real binaries, and that lives in the developer's shell startup file. install
// writes it — with consent, which --yes gives — instead of printing a line to paste.
func TestInstallPutsTheShimsOnPath(t *testing.T) {
	out, zshrc := routedInstall(t, "--yes")
	data, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatalf("no startup file was written: %v\n%s", err, out)
	}
	if !strings.Contains(string(data), `export PATH="`) || !strings.Contains(string(data), `shim/bin:$PATH"`) || !strings.Contains(string(data), "terma shim uninstall") {
		t.Fatalf("the startup file does not put the shims on PATH:\n%s", data)
	}
	if !strings.Contains(out, "~/.zshrc") || !strings.Contains(out, "new terminal") {
		t.Fatalf("install should say where it wrote and what is left to do:\n%s", out)
	}
}

// --no-path keeps install out of the startup file, and the line it prints says the one
// thing a developer placing it by hand has to know: it goes last.
func TestInstallNoPathPrintsTheLineInstead(t *testing.T) {
	out, zshrc := routedInstall(t, "--yes", "--no-path")
	if _, err := os.Stat(zshrc); !os.IsNotExist(err) {
		t.Fatalf("--no-path wrote %s (err=%v)", zshrc, err)
	}
	if !strings.Contains(out, "LAST line") || !strings.Contains(out, "export PATH=") {
		t.Fatalf("install should print the line and where it goes:\n%s", out)
	}
}

// Without a terminal to ask on and without --yes there is no consent, so nothing is
// written: the startup file is the developer's.
func TestInstallDoesNotWriteTheStartupFileUnasked(t *testing.T) {
	out, zshrc := routedInstall(t)
	if _, err := os.Stat(zshrc); !os.IsNotExist(err) {
		t.Fatalf("install wrote %s without being told it could (err=%v)\n%s", zshrc, err, out)
	}
	if !strings.Contains(out, "Not written") {
		t.Fatalf("install should say it did not write, and print the line:\n%s", out)
	}
}

// The wrapper is pasted by hand, so the hint has to name the file the developer's own
// shell reads functions from: a dash or BusyBox ash user told "~/.zshrc (or ~/.bashrc)"
// pastes into a file their shell never reads, and routing is silently off.
func TestWrapperHintNamesTheShellsStartupFile(t *testing.T) {
	for _, tc := range []struct{ shell, want string }{
		{"/bin/zsh", "~/.zshrc"},
		{"/bin/bash", "~/.bashrc"},
		{"/usr/bin/fish", "~/.config/fish/config.fish"},
		{"/bin/dash", "~/.profile"},
		{"/bin/sh", "~/.profile"},
		{"", "~/.profile"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SHELL", tc.shell)
			t.Setenv("ZDOTDIR", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			if got := wrapperFile(filepath.Base(tc.shell)); got != tc.want {
				t.Fatalf("wrapperFile(%q) = %q, want %q", tc.shell, got, tc.want)
			}
		})
	}
}

// install --activation wrapper names one file, the login shell's, not a choice of two.
func TestInstallWrapperHintNamesOneFile(t *testing.T) {
	out, _ := routedInstall(t, "--activation", "wrapper")
	if !strings.Contains(out, "Add these to your ~/.zshrc so the agents route") {
		t.Fatalf("the wrapper hint should name zsh's startup file:\n%s", out)
	}
}
