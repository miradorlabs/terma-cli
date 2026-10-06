package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// A project id from a committed binding must never steer the helper, an executable holding a live
// key, out of the helpers directory.
func TestHelperFilePathRejectsTraversal(t *testing.T) {
	t.Parallel()
	configDir := filepath.Join(t.TempDir(), "cfg")
	for _, id := range []string{
		"../../../../home/.zshenv",
		"../../../../../usr/local/bin/git",
		"..",
		"a/b",
		"/etc/passwd",
	} {
		t.Run(id, func(t *testing.T) {
			if _, err := harness.HelperFilePath(configDir, exporter{}, id); err == nil {
				t.Fatalf("expected %q to be rejected", id)
			}
		})
	}
}

// Whatever a valid id is, the resulting path stays a direct child of the helpers dir.
func TestHelperFilePathStaysInHelpersDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "cfg")
	helpers := filepath.Join(dir, "helpers")
	for _, id := range []string{"770e8400-e29b-41d4-a716-446655440000", "proj_1", "a.b-c"} {
		p, err := harness.HelperFilePath(dir, exporter{}, id)
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		if filepath.Dir(p) != helpers {
			t.Fatalf("%q escaped: %s is not directly under %s", id, p, helpers)
		}
		if !strings.Contains(filepath.Base(p), id) {
			t.Fatalf("%q: expected the id in the file name, got %s", id, filepath.Base(p))
		}
	}
}

// The write itself cannot start outside the helpers directory.
func TestWriteHelperNotReachableOutsideHelpersDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "cfg")
	victim := filepath.Join(tmp, "home", ".zshenv")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# my real shell config\n"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.HelperFilePath(configDir, exporter{}, "../../../../home/.zshenv"); err == nil {
		t.Fatal("expected the traversing id to be refused before any write")
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != original {
		t.Fatalf("victim file was modified: %q (%v)", got, err)
	}
}
