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
// writes it without asking — no --yes and no terminal here — instead of printing a line
// to paste.
func TestInstallPutsTheShimsOnPath(t *testing.T) {
	out, zshrc := routedInstall(t)
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
	// terma cannot change the PATH of the shell that ran it, so the last thing install
	// says is how to make this one read the file.
	if !strings.Contains(out, "Next steps:\n  1. Run `source ~/.zshrc` in this terminal") {
		t.Fatalf("install should end with the command that reloads this shell:\n%s", out)
	}
}

// --no-path keeps install out of the startup file, and the line it prints says the one
// thing a developer placing it by hand has to know: it goes last.
func TestInstallNoPathPrintsTheLineInstead(t *testing.T) {
	out, zshrc := routedInstall(t, "--no-path")
	if _, err := os.Stat(zshrc); !os.IsNotExist(err) {
		t.Fatalf("--no-path wrote %s (err=%v)", zshrc, err)
	}
	if !strings.Contains(out, "LAST line") || !strings.Contains(out, "export PATH=") {
		t.Fatalf("install should print the line and where it goes:\n%s", out)
	}
	if !strings.Contains(out, "! PATH") || !strings.Contains(out, "then run `source ~/.zshrc` in this terminal") {
		t.Fatalf("install should end with what to do with the line:\n%s", out)
	}
}

// A later line that prepends another directory puts the real binaries back in front of
// the shims. install moves its block to the end without asking, and keeps that line.
func TestInstallMovesAnOvertakenPathLine(t *testing.T) {
	_, zshrc := routedInstall(t)
	later := `export PATH="$HOME/.local/bin:$PATH"` + "\n"
	f, err := os.OpenFile(zshrc, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(later); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := within(20*time.Second).combined(t, "install", "--harness", "codex", "--project", "aaaaaaaa-0000-4000-8000-000000000001", "--no-hooks", "--no-doctor", "--no-browser")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	data, err := os.ReadFile(zshrc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), later) {
		t.Fatalf("install dropped the developer's own PATH line:\n%s", data)
	}
	if !strings.HasSuffix(strings.TrimRight(string(data), "\n"), "# <<< terma per-repo routing <<<") {
		t.Fatalf("terma's block is not the last thing in the file:\n%s", data)
	}
	if !strings.Contains(out, "moved terma's line to the end of ~/.zshrc") {
		t.Fatalf("install should say it moved the line:\n%s", out)
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
	if !strings.Contains(out, "so the agents route to this repo's project, then run `source ~/.zshrc` in this terminal") {
		t.Fatalf("install should end with what to do with the functions:\n%s", out)
	}
	// Lines to paste sit four spaces in under their step, the PATH line and the functions
	// alike, so a block reads as one; the step's own text is indented five.
	if !strings.Contains(out, "\n         codex() {") || !strings.Contains(out, "\n         # terma per-repo routing") {
		t.Fatalf("the functions should be indented as a block under their step:\n%s", out)
	}
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

// With no home directory each shell's hint stays its usual file, literally — not a
// relative path, and not ~/.profile for a shell that never reads it.
func TestWrapperHintWithoutAHome(t *testing.T) {
	for _, tc := range []struct{ shell, want string }{
		{"/bin/zsh", "~/.zshrc"},
		{"/bin/bash", "~/.bashrc"},
		{"/usr/bin/fish", "~/.config/fish/config.fish"},
		{"/bin/dash", "~/.profile"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("SHELL", tc.shell)
			t.Setenv("ZDOTDIR", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			if got := wrapperFile(filepath.Base(tc.shell)); got != tc.want {
				t.Fatalf("wrapperFile(%q) without HOME = %q, want %q", tc.shell, got, tc.want)
			}
		})
	}
}
