package shellrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// installScript runs one of install.sh's functions in /bin/sh: the installer repeats
// ShellRC and PathLine there, because it runs before terma is on PATH.
func installScript(t *testing.T, fn string, args ...string) (string, bool) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", `. "$0" && "$@"`, script, fn}, args...)...)
	cmd.Env = append(os.Environ(), "TERMA_INSTALL_FUNCTIONS_ONLY=1")
	out, err := cmd.Output()
	return strings.TrimSuffix(string(out), "\n"), err == nil
}

func TestInstallScriptPicksTheSameStartupFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	home := rcSandbox(t, "zsh")
	check := func(what, shell string) {
		t.Helper()
		t.Setenv("SHELL", "/bin/"+shell)
		if shell == "" {
			t.Setenv("SHELL", "")
		}
		want, wantOK := ShellRC()
		got, ok := installScript(t, "startup_files", shell)
		if ok != wantOK || ok && got != strings.Join(want.Paths, "\n") {
			t.Errorf("%s: install.sh %q %v, ShellRC %q %v", what, got, ok, want.Paths, wantOK)
		}
	}
	for _, shell := range []string{"zsh", "bash", "fish", "tcsh", ""} {
		check("fresh home", shell)
	}
	t.Setenv("ZDOTDIR", filepath.Join(home, "zsh"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	check("ZDOTDIR", "zsh")
	check("XDG_CONFIG_HOME", "fish")
	for _, name := range []string{".profile", ".bash_login", ".bash_profile"} {
		if err := os.WriteFile(filepath.Join(home, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		check("bash with "+name, "bash")
	}
	if os.Geteuid() != 0 { // root reads it anyway
		if err := os.Chmod(filepath.Join(home, ".bash_profile"), 0o200); err != nil {
			t.Fatal(err)
		}
		check("bash with an unreadable .bash_profile", "bash")
	}
}

func TestInstallScriptWritesTheSamePathLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux")
	}
	home := rcSandbox(t, "zsh")
	for _, dir := range []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, `a b$c"d`+"`e`"+`\f`),
		"/opt/terma/bin",
		`/tmp/a$(touch x)b` + "`touch y`" + `"c\d'e`,
	} {
		for _, shell := range []string{"zsh", "bash", "fish"} {
			want := (RC{Shell: shell}).PathLine(dir)
			if got, ok := installScript(t, "path_line", shell, dir); !ok || got != want {
				t.Errorf("%s %q:\ninstall.sh %q\nPathLine   %q", shell, dir, got, want)
			}
		}
	}
}
