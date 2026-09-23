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

// The file is the developer's. Everything in it survives, byte for byte; terma's block
// goes at the end, once; and taking it out again leaves the file as it was found.
func TestRCEnsureIsIdempotentAndRemoveRestoresTheFile(t *testing.T) {
	home := rcSandbox(t, "zsh")
	rc, _ := ShellRC()
	original := "# my zshrc\nexport EDITOR=vim\nexport PATH=\"$HOME/.local/bin:$PATH\"\nalias gs='git status'"
	if err := os.WriteFile(rc.Path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, ".config", "terma", "shim", "bin")

	if state, _ := rc.State(); state != RCAbsent {
		t.Fatalf("state = %v, want absent", state)
	}
	if changed, err := rc.Ensure(binDir); err != nil || !changed {
		t.Fatalf("Ensure: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(rc.Path)
	if !strings.HasPrefix(string(got), original+"\n") {
		t.Fatalf("the developer's own lines changed:\n%s", got)
	}
	if !strings.Contains(string(got), `export PATH="$HOME/.config/terma/shim/bin:$PATH"`) || strings.Count(string(got), rcBegin) != 1 {
		t.Fatalf("block not written once, relative to $HOME:\n%s", got)
	}
	if fi, _ := os.Stat(rc.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file's mode changed to %v", fi.Mode().Perm())
	}
	if state, _ := rc.State(); state != RCLast {
		t.Fatalf("state = %v, want last", state)
	}
	if changed, _ := rc.Ensure(binDir); changed {
		t.Fatal("a second Ensure rewrote a file that needed nothing")
	}

	if removed, err := rc.Remove(); err != nil || !removed {
		t.Fatalf("Remove: %v %v", removed, err)
	}
	if got, _ := os.ReadFile(rc.Path); string(got) != original+"\n" {
		t.Fatalf("Remove did not leave the file as it was found:\n%q\nwant\n%q", got, original+"\n")
	}
	if removed, _ := rc.Remove(); removed {
		t.Fatal("nothing left to remove")
	}
}

// The trap this exists for: another installer appends its own PATH line later, and the
// real binaries are back in front. That is noticed, and the block moves to the end.
func TestRCNoticesBeingOvertakenAndMovesToTheEnd(t *testing.T) {
	home := rcSandbox(t, "zsh")
	rc, _ := ShellRC()
	binDir := filepath.Join(home, ".config", "terma", "shim", "bin")
	if _, err := rc.Ensure(binDir); err != nil {
		t.Fatal(err)
	}
	appendTo := func(s string) {
		t.Helper()
		f, err := os.OpenFile(rc.Path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}

	// Lines that do not set PATH leave terma's block in charge of it.
	appendTo("\n# a comment mentioning export PATH=/nowhere\nexport GOPATH=\"$HOME/go\"\nexport MANPATH=\"/usr/local/man:$MANPATH\"\nexport PNPM_HOME=\"$HOME/pnpm\"\n")
	if state, _ := rc.State(); state != RCLast {
		t.Fatalf("GOPATH, MANPATH and a comment are not PATH: state = %v", state)
	}

	for _, overtaking := range []string{
		"\n# Added by Antigravity CLI installer\nexport PATH=\"$HOME/.local/bin:$PATH\"\n",
		"\npath=($HOME/.local/bin $path)\n",
		"\ncase \":$PATH:\" in *) ;; esac; PATH=\"$HOME/bin:$PATH\"\n",
	} {
		appendTo(overtaking)
		if state, _ := rc.State(); state != RCOvertaken {
			t.Fatalf("%q after the block: state = %v, want overtaken", overtaking, state)
		}
		if changed, err := rc.Ensure(binDir); err != nil || !changed {
			t.Fatalf("Ensure: %v %v", changed, err)
		}
		got, _ := os.ReadFile(rc.Path)
		if strings.Count(string(got), rcBegin) != 1 || !strings.HasSuffix(string(got), rcEnd+"\n") || !strings.Contains(string(got), strings.TrimSpace(overtaking)) {
			t.Fatalf("the block should be last, once, with the other line kept:\n%s", got)
		}
	}
}

// What the block is for, checked the only way that counts: a shell reads the file, and the
// agent's name resolves to terma's shim although ~/.local/bin is prepended twice before it.
func TestRCBlockPutsTheShimFirstInARealShell(t *testing.T) {
	home := rcSandbox(t, "bash")
	binDir := filepath.Join(home, ".config", "terma", "shim", "bin")
	realDir := filepath.Join(home, ".local", "bin")
	writeExe(t, filepath.Join(binDir, "codex"))
	writeExe(t, filepath.Join(realDir, "codex"))
	rc := RC{Path: filepath.Join(home, ".profile"), Shell: "bash"}
	prepend := "export PATH=\"$HOME/.local/bin:$PATH\"\n"
	if err := os.WriteFile(rc.Path, []byte(prepend+prepend), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func() string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", `. "$HOME/.profile"; command -v codex`)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := resolve(); got != filepath.Join(realDir, "codex") {
		t.Fatalf("before: %s", got)
	}
	if _, err := rc.Ensure(binDir); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != filepath.Join(binDir, "codex") {
		t.Fatalf("after Ensure the shim should win, got %s", got)
	}
	// Overtaken by a later installer, and put right again.
	f, _ := os.OpenFile(rc.Path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(prepend)
	f.Close()
	if got := resolve(); got != filepath.Join(realDir, "codex") {
		t.Fatalf("a later prepend should overtake the block (that is the trap), got %s", got)
	}
	if _, err := rc.Ensure(binDir); err != nil {
		t.Fatal(err)
	}
	if got := resolve(); got != filepath.Join(binDir, "codex") {
		t.Fatalf("after moving the block to the end the shim should win again, got %s", got)
	}
}

// Dotfiles are often symlinks into a repository. The link must survive.
func TestRCWritesThroughASymlink(t *testing.T) {
	home := rcSandbox(t, "zsh")
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "zshrc")
	if err := os.WriteFile(target, []byte("export EDITOR=vim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	rc, _ := ShellRC()
	if _, err := rc.Ensure(filepath.Join(home, ".config/terma/shim/bin")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(rc.Path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a file (err=%v)", err)
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), rcBegin) {
		t.Fatalf("the block did not reach the file the link points at:\n%s", got)
	}
}

// `terma shim uninstall` takes the block out with everything else.
func TestRemoveAllTakesThePathBlockOut(t *testing.T) {
	home := rcSandbox(t, "zsh")
	rc, _ := ShellRC()
	if err := os.WriteFile(rc.Path, []byte("export EDITOR=vim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Ensure(filepath.Join(home, ".config/terma/shim/bin")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(rc.Path); string(got) != "export EDITOR=vim\n" {
		t.Fatalf("uninstall left the file as:\n%q", got)
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
