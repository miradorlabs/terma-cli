package cmd

import (
	"os"
	"path/filepath"
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
