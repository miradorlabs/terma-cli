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
	path := filepath.Join(dir, "terma")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorComparesPATHBinaryWithRunningBuild(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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

// Two copies are only a problem when they disagree: a second build somewhere an agent
// started outside this shell would find first is reported; the same build, a link to this
// file, a non-executable and terma's own shim directory are not.
func TestOtherTermasReportsOnlyADifferentBuild(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
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

	// A stale copy where PATH does not look — the system directory an app started from
	// the Dock searches first — found through the well-known list.
	stale := writeTerma(t, t.TempDir(), "build B, from this morning")
	others := otherTermas(primary, []string{filepath.Dir(stale), filepath.Dir(stale)})
	if len(others) != 1 || !strings.Contains(others[0], filepath.Base(filepath.Dir(stale))) || !strings.Contains(others[0], "installed ") {
		t.Fatalf("the stale copy should be reported once, with when it was installed: %v", others)
	}
}

// A build run from a directory that is not on PATH — `make build` and then bin/terma —
// installs hooks that call `terma` by name and find nothing. The fix is the command that
// puts the directory on PATH, quoted once and whole, so it is drawn as one command to copy.
func TestDoctorGivesTheCommandThatPutsTermaOnPath(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", "")
	t.Setenv("PATH", t.TempDir())
	current := writeTerma(t, filepath.Join(t.TempDir(), "it's a build"), "this build")
	dir := filepath.Dir(current)

	check := BinaryCheck(current, nil)
	command := AddToPathCommand(dir)
	if check.Status != Fail || !strings.Contains(check.Fix, "run `"+command+"` to put "+dir+" on PATH") || strings.Count(check.Fix, "`") != 2 {
		t.Fatalf("the fix should be the one quoted command: %+v", check)
	}
	echo, reload, ok := strings.Cut(command, " && ")
	if !ok || reload != "source ~/.zshrc" || !strings.HasSuffix(echo, " >> ~/.zshrc") {
		t.Fatalf("zsh: %q, want the line appended to ~/.zshrc, then sourced", command)
	}
	// The quoting holds for a directory with a quote and a space in it: the line lands in
	// the file exactly as terma writes a PATH line for this shell.
	if out, err := exec.Command("/bin/sh", "-c", echo).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", echo, err, out)
	}
	rc, _ := shellrc.ShellRC()
	if data, _ := os.ReadFile(filepath.Join(home, ".zshrc")); string(data) != rc.PathLine(dir)+"\n" {
		t.Fatalf("~/.zshrc = %q, want %q", data, rc.PathLine(dir)+"\n")
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
