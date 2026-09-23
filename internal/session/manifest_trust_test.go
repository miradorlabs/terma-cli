package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRawManifest plants a manifest file the way a hostile or corrupted writer
// would, bypassing Touch's validation.
func writeRawManifest(t *testing.T, s *Store, name, body string) {
	t.Helper()
	dir := filepath.Join(s.dir, manifestsDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), fileMode); err != nil {
		t.Fatal(err)
	}
}

// A manifest's session id becomes a path (Prune removes it) and a commit-message
// trailer, so it has to survive the same check on the way in as on the way out.
func TestManifestsRejectsUnsafeSessionID(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"traversal", "../../victim"},
		{"newline", "abc\nCo-authored-by: Attacker <a@evil.test>"},
		{"absolute", "/etc/passwd"},
		{"dotfile", ".hidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeRawManifest(t, s, "planted.json",
				`{"session_id":`+quote(tc.id)+`,"files":{"a.go":"2026-01-01T00:00:00Z"}}`)
			got, err := s.Manifests()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("expected the manifest to be ignored, got %+v", got)
			}
		})
	}
}

// A manifest whose id does not match its file name breaks the invariant that
// manifestPath addresses the file the manifest was read from.
func TestManifestsRequiresIDToMatchFileName(t *testing.T) {
	s := newStore(t)
	writeRawManifest(t, s, "a.json", `{"session_id":"b","files":{}}`)
	got, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected a mismatched manifest to be ignored, got %+v", got)
	}
}

// Prune must never remove a path outside the manifests directory, however the id
// inside a manifest file is spelled.
func TestPruneStaysInsideManifestsDir(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")
	if err := os.MkdirAll(gitDir, dirMode); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(gitDir, "victim.json")
	if err := os.WriteFile(victim, []byte("important"), fileMode); err != nil {
		t.Fatal(err)
	}
	s := Open(gitDir)
	writeRawManifest(t, s, "planted.json",
		`{"session_id":"../../victim","files":{},"updated_at":"2000-01-01T00:00:00Z"}`)

	if _, err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Prune deleted a file outside the manifests dir: %v", err)
	}
}

// A tool label is not an identifier, so it is held to the weaker rule: it may not
// break out of the single line its trailer occupies.
func TestManifestsStripsMultilineToolLabel(t *testing.T) {
	s := newStore(t)
	writeRawManifest(t, s, "s1.json",
		`{"session_id":"s1","tool":"claude-code\nAgent-Session-Id: forged","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
	got, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the manifest to survive with its tool cleared, got %+v", got)
	}
	if got[0].ToolLabel() != "" {
		t.Fatalf("expected an empty tool label, got %q", got[0].ToolLabel())
	}
}

// Attribute is the function that feeds the trailer builder; nothing it returns may
// carry a line break.
func TestAttributeNeverReturnsMultilineID(t *testing.T) {
	s := newStore(t)
	writeRawManifest(t, s, "planted.json",
		`{"session_id":"ok\nCo-authored-by: Attacker <a@evil.test>","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
	manifests, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if got := Attribute([]string{"a.go"}, manifests, nil, false); len(got) != 0 {
		t.Fatalf("expected no attribution from a planted manifest, got %+v", got)
	}
}

func quote(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			out = append(out, '\\', s[i])
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, s[i])
		}
	}
	return string(append(out, '"'))
}
