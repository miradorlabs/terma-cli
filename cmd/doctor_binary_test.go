package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
)

// What else is installed on the machine running the tests is none of their business.
func init() { wellKnownBinDirs = func() []string { return nil } }

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
	check := doctorBinaryCheckFor(current)
	if check.Status != doctor.Warn || !strings.Contains(check.Detail, "hooks run a different build") || !strings.Contains(check.Fix, filepath.Dir(current)) {
		t.Fatalf("stale hook binary must be reported: %+v", check)
	}
	if err := os.WriteFile(installed, []byte("new build"), 0755); err != nil {
		t.Fatal(err)
	}
	if check := doctorBinaryCheckFor(current); check.Status != doctor.Pass {
		t.Fatalf("identical copy should pass: %+v", check)
	}
}

func TestDoctorRecognizesOfficialNpmLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("npm's Windows command wrapper has a different layout")
	}
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	launcher, err := os.ReadFile(filepath.Join("..", "npm", "bin", "terma.js"))
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
	if check := doctorBinaryCheckFor(current); check.Status != doctor.Pass {
		t.Fatalf("official npm launcher should delegate to this build: %+v", check)
	}
	if others := otherTermas(filepath.Join(bin, "terma")); len(others) != 0 {
		t.Fatalf("npm launcher should compare its vendor binary: %v", others)
	}
	if err := os.WriteFile(vendor, []byte("old Go build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if check := doctorBinaryCheckFor(current); check.Status != doctor.Warn {
		t.Fatalf("stale vendor binary must still be reported: %+v", check)
	}
	if err := os.WriteFile(vendor, []byte("same Go build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "bin", "terma.js"), append(launcher, []byte("\n// changed\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	if check := doctorBinaryCheckFor(current); check.Status != doctor.Warn {
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
	if others := otherTermas(primary); len(others) != 0 {
		t.Fatalf("nothing here disagrees with the primary: %v", others)
	}

	// A stale copy where PATH does not look — the system directory an app started from
	// the Dock searches first — found through the well-known list.
	stale := writeTerma(t, t.TempDir(), "build B, from this morning")
	saved := wellKnownBinDirs
	wellKnownBinDirs = func() []string { return []string{filepath.Dir(stale), filepath.Dir(stale)} }
	t.Cleanup(func() { wellKnownBinDirs = saved })
	others := otherTermas(primary)
	if len(others) != 1 || !strings.Contains(others[0], filepath.Base(filepath.Dir(stale))) || !strings.Contains(others[0], "installed ") {
		t.Fatalf("the stale copy should be reported once, with when it was installed: %v", others)
	}
}
