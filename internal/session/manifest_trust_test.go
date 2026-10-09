package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRawManifest plants a manifest file in codex's folder, bypassing Touch's validation.
func writeRawManifest(t *testing.T, s *Store, name, body string) {
	t.Helper()
	dir := filepath.Join(s.dir, manifestsDir, "codex")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), fileMode); err != nil {
		t.Fatal(err)
	}
}

// A manifest's session id is validated on read as on write: it becomes a path and a trailer.
func TestManifestsRejectsUnsafeSessionID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, id string }{
		{"traversal", "../../victim"},
		{"newline", "abc\nCo-authored-by: Attacker <a@evil.test>"},
		{"absolute", "/etc/passwd"},
		{"dotfile", ".hidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeRawManifest(t, s, "planted.json",
				`{"session_id":`+quote(tc.id)+`,"tool":"codex","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
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

// A manifest whose id does not match its file name, or whose tool its folder, is skipped.
func TestManifestsRequiresKeyToMatchPath(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeRawManifest(t, s, "a.json", `{"session_id":"b","tool":"codex","files":{}}`)
	writeRawManifest(t, s, "c.json", `{"session_id":"c","tool":"claude-code","files":{}}`)
	got, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected a mismatched manifest to be ignored, got %+v", got)
	}
}

// Prune never removes a path outside the manifests directory.
func TestPruneStaysInsideManifestsDir(t *testing.T) {
	t.Parallel()
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
		`{"session_id":"../../victim","tool":"codex","files":{},"updated_at":"2000-01-01T00:00:00Z"}`)

	if _, err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Prune deleted a file outside the manifests dir: %v", err)
	}
}

// A tool label may not break out of its trailer line: a multi-line tool names no folder,
// and a multi-line version is dropped.
func TestManifestsStripsMultilineToolLabel(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeRawManifest(t, s, "s1.json",
		`{"session_id":"s1","tool":"codex\nAgent-Session-Id: forged","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
	writeRawManifest(t, s, "s2.json",
		`{"session_id":"s2","tool":"codex","tool_version":"1\nAgent-Session-Id: forged","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
	got, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "s2" {
		t.Fatalf("expected only s2 to survive, got %+v", got)
	}
	if got[0].ToolLabel() != "codex" {
		t.Fatalf("expected the version dropped, got %q", got[0].ToolLabel())
	}
}

// Nothing Attribute returns carries a line break.
func TestAttributeNeverReturnsMultilineID(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	writeRawManifest(t, s, "planted.json",
		`{"session_id":"ok\nCo-authored-by: Attacker <a@evil.test>","tool":"codex","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
	manifests, err := s.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	if got := Attribute([]string{"a.go"}, manifests); len(got) != 0 {
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

// A delta is trusted no more than a manifest: its id must be safe and match its file name.
func TestDeltasRequireASafeIDMatchingTheirFileName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, file, id string }{
		{"another session", "a~0001.delta", "b"},
		{"traversal", "x~0001.delta", "../../victim"},
		{"newline", "x~0001.delta", "x\nCo-authored-by: Attacker <a@evil.test>"},
		{"no separator", "x.delta", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			writeRawManifest(t, s, tc.file, `{"session_id":`+quote(tc.id)+`,"tool":"codex","files":{"a.go":"2026-01-01T00:00:00Z"}}`)
			got, err := s.Manifests()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("expected the delta to be ignored, got %+v", got)
			}
		})
	}
}
