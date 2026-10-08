package doctor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/shellrc"
)

func writeTerma(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "terma"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorComparesPATHBinaryWithRunningBuild(t *testing.T) {
	current := writeTerma(t, t.TempDir(), "new build")
	installed := writeTerma(t, t.TempDir(), "old build")
	t.Setenv("PATH", filepath.Dir(installed))
	check := BinaryCheck(current, nil)
	if check.Status != Warn || !strings.Contains(check.Detail, "hooks run a different build") || !strings.Contains(check.Fix, filepath.Dir(current)) {
		t.Fatalf("stale hook binary must be reported: %+v", check)
	}
	if err := os.WriteFile(installed, []byte("new build"), 0755); err != nil {
		t.Fatal(err)
	}
	if check := BinaryCheck(current, nil); check.Status != Pass {
		t.Fatalf("identical copy should pass: %+v", check)
	}
}

func TestDoctorRecognizesOfficialNpmLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("npm's Windows command wrapper has a different layout")
	}
	launcher, err := os.ReadFile(filepath.Join("..", "..", "npm", "bin", "terma.js"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(launcher)
	if hex.EncodeToString(sum[:]) != npmLauncherDigest {
		t.Fatal("the npm launcher changed; update the pinned digest only after reviewing its vendor-binary delegation")
	}
	current := writeTerma(t, t.TempDir(), "same Go build")
	pkg := filepath.Join(t.TempDir(), "lib", "node_modules", "@miradorlabs", "terma")
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin", "terma.js"), launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	vendor := writeTerma(t, filepath.Join(pkg, "vendor"), "same Go build")
	bin := t.TempDir()
	if err := os.Symlink(filepath.Join(pkg, "bin", "terma.js"), filepath.Join(bin, "terma")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if check := BinaryCheck(current, nil); check.Status != Pass {
		t.Fatalf("official npm launcher should delegate to this build: %+v", check)
	}
	if others := otherTermas(filepath.Join(bin, "terma"), nil); len(others) != 0 {
		t.Fatalf("npm launcher should compare its vendor binary: %v", others)
	}
	if err := os.WriteFile(vendor, []byte("old Go build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if check := BinaryCheck(current, nil); check.Status != Warn {
		t.Fatalf("stale vendor binary must still be reported: %+v", check)
	}
	if err := os.WriteFile(vendor, []byte("same Go build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin", "terma.js"), append(launcher, []byte("\n// changed\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if check := BinaryCheck(current, nil); check.Status != Warn {
		t.Fatalf("edited npm launcher must not be trusted: %+v", check)
	}
}

// Only a different build is reported, not the same build, a link or a non-executable.
func TestOtherTermasReportsOnlyADifferentBuild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links and executable bits are Unix's")
	}
	primary := writeTerma(t, t.TempDir(), "build A")

	sameBuild := filepath.Dir(writeTerma(t, t.TempDir(), "build A"))
	linked := t.TempDir()
	if err := os.Symlink(primary, filepath.Join(linked, "terma")); err != nil {
		t.Fatal(err)
	}
	notExecutable := t.TempDir()
	if err := os.WriteFile(filepath.Join(notExecutable, "terma"), []byte("build B"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(primary), sameBuild, linked, notExecutable, "", t.TempDir()}, string(os.PathListSeparator)))
	if others := otherTermas(primary, nil); len(others) != 0 {
		t.Fatalf("nothing here disagrees with the primary: %v", others)
	}

	// A stale copy where PATH does not look, found through the well-known list.
	stale := writeTerma(t, t.TempDir(), "build B, from this morning")
	others := otherTermas(primary, []string{filepath.Dir(stale), filepath.Dir(stale)})
	if len(others) != 1 || !strings.Contains(others[0], filepath.Base(filepath.Dir(stale))) || !strings.Contains(others[0], "installed ") {
		t.Fatalf("the stale copy should be reported once, with when it was installed: %v", others)
	}
}

// A build off PATH needs nothing, and the command that puts a directory on PATH is one quoted line.
func TestDoctorGivesTheCommandThatPutsTermaOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command is for a Unix shell rc file")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("PATH", t.TempDir())
	current := writeTerma(t, filepath.Join(t.TempDir(), "it's a build"), "this build")
	dir := filepath.Dir(current)

	// The hooks run terma by its full path, so they need nothing from PATH.
	if check := BinaryCheck(current, nil); check.Status != Pass || !strings.Contains(check.Detail, "full path setup wrote") {
		t.Fatalf("the hooks must not need PATH: %+v", check)
	}
	command := AddToPathCommand(dir)
	echo, reload, ok := strings.Cut(command, " && ")
	if !ok || reload != "source ~/.zshrc" || !strings.HasSuffix(echo, " >> ~/.zshrc") {
		t.Fatalf("zsh: %q, want the line appended to ~/.zshrc, then sourced", command)
	}
	// The quoting holds for a directory with a quote and a space in it.
	if out, err := exec.Command("/bin/sh", "-c", echo).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", echo, err, out)
	}
	rc, _ := shellrc.ShellRC()
	if data, _ := os.ReadFile(filepath.Join(home, ".zshrc")); string(data) != rc.PathLine(dir)+"\n" {
		t.Fatalf("~/.zshrc = %q, want %q", data, rc.PathLine(dir)+"\n")
	}

	// bash: the interactive and the login startup file, then the interactive one sourced.
	t.Setenv("SHELL", "/bin/bash")
	line := `'export PATH="/opt/terma:$PATH"'`
	if got, want := AddToPathCommand("/opt/terma"), "echo "+line+" >> ~/.bashrc && echo "+line+" >> ~/.profile && source ~/.bashrc"; got != want {
		t.Errorf("bash: %q, want %q", got, want)
	}

	t.Setenv("SHELL", "/usr/bin/fish")
	if got, want := AddToPathCommand("/opt/terma"), `fish_add_path --move --prepend "/opt/terma"`; got != want {
		t.Errorf("fish: %q, want %q", got, want)
	}
	t.Setenv("SHELL", "/bin/dash")
	if got, want := AddToPathCommand("/opt/terma"), `export PATH="/opt/terma:$PATH"`; got != want {
		t.Errorf("an unknown shell: %q, want %q", got, want)
	}
}
