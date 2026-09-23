package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A project id out of a committed .terma.toml must never steer the helper — which
// holds a live server key and is written executable — out of the helpers directory.
func TestHelperFilePathRejectsTraversal(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "cfg"))
	for _, id := range []string{
		"../../../../home/.zshenv",
		"../../../../../usr/local/bin/git",
		"..",
		"a/b",
		"/etc/passwd",
	} {
		t.Run(id, func(t *testing.T) {
			if _, err := HelperFilePath(Claude{}, id); err == nil {
				t.Fatalf("expected %q to be rejected", id)
			}
		})
	}
}

// Whatever a valid id is, the resulting path stays a direct child of the helpers dir.
func TestHelperFilePathStaysInHelpersDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("TERMA_CONFIG_DIR", dir)
	helpers := filepath.Join(dir, "helpers")
	for _, id := range []string{"770e8400-e29b-41d4-a716-446655440000", "proj_1", "a.b-c"} {
		p, err := HelperFilePath(Claude{}, id)
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

// The regression proper: the write that used to land anywhere now cannot start.
func TestWriteHelperNotReachableOutsideHelpersDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(tmp, "cfg"))
	victim := filepath.Join(tmp, "home", ".zshenv")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "# my real shell config\n"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := HelperFilePath(Claude{}, "../../../../home/.zshenv"); err == nil {
		t.Fatal("expected the traversing id to be refused before any write")
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != original {
		t.Fatalf("victim file was modified: %q (%v)", got, err)
	}
}
