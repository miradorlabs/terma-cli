package shellrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// rcSandbox gives the test a home directory of its own and a login shell to claim.
func rcSandbox(t *testing.T, shell string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	t.Setenv("SHELL", "/bin/"+shell)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

func TestShellRCNamesTheFilesTheShellReads(t *testing.T) {
	home := rcSandbox(t, "zsh")
	if rc, ok := ShellRC(); !ok || !slices.Equal(rc.Paths, []string{filepath.Join(home, ".zshrc")}) || rc.Shell != "zsh" {
		t.Fatalf("zsh: %+v %v", rc, ok)
	}
	zdot := t.TempDir()
	t.Setenv("ZDOTDIR", zdot)
	if rc, _ := ShellRC(); !slices.Equal(rc.Paths, []string{filepath.Join(zdot, ".zshrc")}) {
		t.Fatalf("zsh honours ZDOTDIR: %+v", rc)
	}

	// An interactive bash reads .bashrc; a login bash reads the first of .bash_profile,
	// .bash_login and .profile that exists and is readable. Both get the line, on every OS.
	t.Setenv("SHELL", "/usr/local/bin/bash")
	for _, step := range []struct {
		create string
		mode   os.FileMode
		login  string
	}{
		{"", 0, ".profile"},
		{".bash_login", 0o644, ".bash_login"},
		{".bash_profile", 0o200, ".bash_login"}, // write-only: bash skips it
		{".bash_profile", 0o644, ".bash_profile"},
	} {
		if step.mode == 0o200 && os.Geteuid() == 0 {
			continue // root reads it anyway
		}
		if step.create != "" {
			path := filepath.Join(home, step.create)
			if err := os.WriteFile(path, nil, step.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, step.mode); err != nil {
				t.Fatal(err)
			}
		}
		want := []string{filepath.Join(home, ".bashrc"), filepath.Join(home, step.login)}
		if rc, ok := ShellRC(); !ok || !slices.Equal(rc.Paths, want) {
			t.Fatalf("bash with %q: %+v %v, want %q", step.create, rc, ok, want)
		}
	}

	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	if rc, ok := ShellRC(); !ok || !slices.Equal(rc.Paths, []string{filepath.Join(home, ".config", "fish", "conf.d", "terma.fish")}) {
		t.Fatalf("fish: %+v %v", rc, ok)
	}
	if line := (RC{Shell: "fish"}).PathLine(filepath.Join(home, ".local/bin")); line != `fish_add_path --move --prepend "$HOME/.local/bin"` {
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

// TestPathLineEscapesHostilePaths: a path with shell metacharacters, sourced in /bin/sh,
// runs nothing and puts the literal directory on PATH.
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
