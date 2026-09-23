package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The project id in this file becomes a path component (harness.HelperFilePath), and
// the file is committed — so it arrives from whoever wrote the repository.
func TestLoadRejectsUnsafeProjectID(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"traversal", "../../../../home/.zshenv"},
		{"absolute", "/etc/cron.d/terma"},
		{"slash", "a/b"},
		{"dotdot", ".."},
		{"newline", "abc\ndef"},
		{"space", "my project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeID(t, root, tc.id)
			if _, err := Load(root); err == nil {
				t.Fatalf("expected %q to be rejected", tc.id)
			} else if !strings.Contains(err.Error(), "invalid project id") {
				t.Fatalf("expected an invalid-id error, got %v", err)
			}
		})
	}
}

// The shapes the backend actually issues must keep working.
func TestLoadAcceptsRealProjectIDs(t *testing.T) {
	for _, id := range []string{
		"770e8400-e29b-41d4-a716-446655440000",
		"proj_123",
		"project-a",
		"abc.def",
	} {
		t.Run(id, func(t *testing.T) {
			root := t.TempDir()
			writeID(t, root, id)
			f, err := Load(root)
			if err != nil {
				t.Fatalf("expected %q to be accepted, got %v", id, err)
			}
			if f.Project.ID != id {
				t.Fatalf("got %q, want %q", f.Project.ID, id)
			}
		})
	}
}

func TestValidID(t *testing.T) {
	if ValidID("") || ValidID(strings.Repeat("a", maxIDLen+1)) {
		t.Fatal("empty and over-long ids must be rejected")
	}
	if !ValidID("a") {
		t.Fatal("a minimal id must be accepted")
	}
}

func writeID(t *testing.T, root, id string) {
	t.Helper()
	body, err := json.Marshal(File{Project: Project{ID: id}})
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, root, body)
}

func writeRaw(t *testing.T, root string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root), body, 0o644); err != nil {
		t.Fatal(err)
	}
}
