package shim

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// rcSandbox gives the test a home directory of its own and a login shell to claim.
func rcSandbox(t *testing.T, shell string) string {
	t.Helper()
	sandbox(t)
	home, _ := os.UserHomeDir()
	t.Setenv("SHELL", "/bin/"+shell)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestShellRCNamesTheFileTheShellReads(t *testing.T) {
	home := rcSandbox(t, "zsh")
	if rc, ok := ShellRC(); !ok || rc.Path != filepath.Join(home, ".zshrc") || rc.Shell != "zsh" {
		t.Fatalf("zsh: %+v %v", rc, ok)
	}
	zdot := t.TempDir()
	t.Setenv("ZDOTDIR", zdot)
	if rc, _ := ShellRC(); rc.Path != filepath.Join(zdot, ".zshrc") {
		t.Fatalf("zsh honours ZDOTDIR: %+v", rc)
	}

	t.Setenv("SHELL", "/usr/local/bin/bash")
	if rc, ok := ShellRC(); !ok || rc.Path != filepath.Join(home, ".bashrc") {
		t.Fatalf("bash: %+v %v", rc, ok)
	}
	// A macOS terminal starts a login shell, which reads .bash_profile and never .bashrc.
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want := ".bashrc"
	if runtime.GOOS == "darwin" {
		want = ".bash_profile"
	}
	if rc, _ := ShellRC(); filepath.Base(rc.Path) != want {
		t.Fatalf("bash on %s: %s, want %s", runtime.GOOS, rc.Path, want)
	}

	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	if rc, ok := ShellRC(); !ok || rc.Path != filepath.Join(home, ".config", "fish", "conf.d", "terma.fish") {
		t.Fatalf("fish: %+v %v", rc, ok)
	}
	if line := (RC{Shell: "fish"}).PathLine(filepath.Join(home, ".config/terma/shim/bin")); line != `fish_add_path --move --prepend "$HOME/.config/terma/shim/bin"` {
		t.Fatalf("fish line: %s", line)
	}

	// A shell terma cannot write for is said so, never guessed at.
	for _, shell := range []string{"/bin/tcsh", "/usr/bin/nu", ""} {
		t.Setenv("SHELL", shell)
		if rc, ok := ShellRC(); ok {
			t.Fatalf("%q: claimed %+v", shell, rc)
		}
	}
}

// sandbox points config.Dir() and the home directory at temporary directories so a test
// never reads or writes real state.
func sandbox(t *testing.T) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// legacyBlock is the block earlier builds wrote at the end of a startup file.
func legacyBlock(rc RC, binDir string) string {
	return rcBegin + "\n# Keeps terma's shims ahead of the real claude and codex.\n" + rc.PathLine(binDir) + "\n" + rcEnd + "\n"
}

// Removing the block an earlier build wrote leaves the developer's file as it was, byte
// for byte, the blank line that led into the block included; a file without the block
// is not touched.
func TestRCRemoveRestoresTheFile(t *testing.T) {
	home := rcSandbox(t, "zsh")
	rc, _ := ShellRC()
	mine := "export EDITOR=vim\nexport PATH=\"$HOME/.local/bin:$PATH\"\n"
	if err := os.WriteFile(rc.Path, []byte(mine+"\n"+legacyBlock(rc, filepath.Join(home, ".config/terma/shim/bin"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := rc.Remove(); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if got, _ := os.ReadFile(rc.Path); string(got) != mine {
		t.Fatalf("the file after removal:\n%q\nwant\n%q", got, mine)
	}
	if removed, err := rc.Remove(); err != nil || removed {
		t.Fatalf("a second removal changed something: %v %v", removed, err)
	}
}

// Dotfiles are often symlinks into a repository: removal writes through the link.
func TestRCRemoveWritesThroughASymlink(t *testing.T) {
	home := rcSandbox(t, "zsh")
	target := filepath.Join(t.TempDir(), "zshrc")
	rc, _ := ShellRC()
	if err := os.WriteFile(target, []byte("export EDITOR=vim\n\n"+legacyBlock(rc, filepath.Join(home, ".config/terma/shim/bin"))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, rc.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Remove(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(rc.Path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a file (err=%v)", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "export EDITOR=vim\n" {
		t.Fatalf("the linked file after removal: %q", got)
	}
}

// RemoveLegacy takes the scripts, the PATH block and Claude's route documents away, and
// keeps the routing records: they are the relay's policy now.
func TestRemoveLegacyKeepsTheRoutingRecords(t *testing.T) {
	home := rcSandbox(t, "zsh")
	binDir, _ := ShimBinDir()
	base := filepath.Dir(filepath.Dir(binDir))
	for _, f := range []string{filepath.Join(binDir, "claude"), filepath.Join(base, "claude", "p1", "settings.json"), filepath.Join(base, "routing", "p1.json")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rc, _ := ShellRC()
	if err := os.WriteFile(rc.Path, []byte("export EDITOR=vim\n\n"+legacyBlock(rc, binDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveLegacy()
	if err != nil || !removed {
		t.Fatalf("RemoveLegacy: %v %v", removed, err)
	}
	for _, gone := range []string{filepath.Dir(binDir), filepath.Join(base, "claude")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s survived", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "routing", "p1.json")); err != nil {
		t.Errorf("the routing record went: %v", err)
	}
	if got, _ := os.ReadFile(rc.Path); string(got) != "export EDITOR=vim\n" {
		t.Errorf("the startup file: %q", got)
	}
	_ = home
	if removed, err := RemoveLegacy(); err != nil || removed {
		t.Fatalf("a second removal found something: %v %v", removed, err)
	}
}

// A legacy script's preparation gets an empty plan, which the script reads as "start
// the agent as it is"; the oldest scripts' exec finds the real binary past the shims.
func TestLegacyScriptsStillStartTheAgent(t *testing.T) {
	sandbox(t)
	plan := t.TempDir()
	if err := PrepareNothing(plan); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(plan, "count")); string(got) != "terma-args-v1:0\n" {
		t.Fatalf("plan: %q", got)
	}
	if PrepareNothing(filepath.Join(plan, "missing")) == nil {
		t.Fatal("a plan directory that is not there was accepted")
	}
	binDir, _ := ShimBinDir()
	realDir := t.TempDir()
	for _, d := range []string{binDir, realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+"/"+string(os.PathListSeparator)+realDir)
	if got, err := RealBinary("codex"); err != nil || got != filepath.Join(realDir, "codex") {
		t.Fatalf("RealBinary = %q, %v; want the one past the shims", got, err)
	}
}

// TestPathLineEscapesHostilePaths proves a shim directory whose path contains shell
// metacharacters is written inert: sourced in /bin/sh it neither runs a command
// substitution nor breaks the line, and PATH still receives the literal directory.
func TestPathLineEscapesHostilePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell rc")
	}
	t.Run("outside home", func(t *testing.T) {
		dir := t.TempDir()
		sentinel := filepath.Join(dir, "pwned")
		hostile := filepath.Join(dir, "a$(touch "+sentinel+")b`touch "+sentinel+"`")
		line := (RC{Shell: "bash"}).PathLine(hostile)
		out, err := exec.Command("/bin/sh", "-c", line+`; printf '%s' "$PATH"`).CombinedOutput()
		if err != nil {
			t.Fatalf("sourcing failed: %v\n%s\nline: %s", err, out, line)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("command substitution executed; line: %s", line)
		}
		if !strings.HasPrefix(string(out), hostile+":") {
			t.Fatalf("PATH not the literal dir:\n got %q\nwant prefix %q\nline %s", out, hostile+":", line)
		}
	})
	t.Run("under home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		sentinel := filepath.Join(home, "pwned")
		hostile := filepath.Join(home, "a$(touch "+sentinel+")")
		line := (RC{Shell: "bash"}).PathLine(hostile)
		if !strings.Contains(line, "$HOME/") {
			t.Fatalf("home-relative path lost its $HOME prefix: %s", line)
		}
		out, err := exec.Command("/bin/sh", "-c", `HOME=`+home+`; `+line+`; printf '%s' "$PATH"`).CombinedOutput()
		if err != nil {
			t.Fatalf("sourcing failed: %v\n%s\nline: %s", err, out, line)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("command substitution executed; line: %s", line)
		}
		if !strings.HasPrefix(string(out), hostile+":") {
			t.Fatalf("PATH not the literal dir:\n got %q\nwant prefix %q\nline %s", out, hostile+":", line)
		}
	})
	t.Run("fish escapes dollar and quote, not backtick", func(t *testing.T) {
		line := (RC{Shell: "fish"}).PathLine(`/opt/a$b"c` + "`d")
		if !strings.Contains(line, `\$b`) || !strings.Contains(line, `\"c`) {
			t.Fatalf("fish line did not escape $ or \": %s", line)
		}
		if strings.Contains(line, "\\`") {
			t.Fatalf("fish line escaped a backtick it should leave literal: %s", line)
		}
	})
}
