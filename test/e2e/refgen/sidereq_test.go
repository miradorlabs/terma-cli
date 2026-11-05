package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A snapshot published while an empty side_requests was left out reads as observed and empty
// where its first turn was captured, and stays not observed where it was not.
func TestLoadEmptySideRequests(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"2.1.318.json": `{"schema_version":1,"harness":"claude","version":"2.1.318","prompt":{"system":[]}}`,
		"2.1.100.json": `{"schema_version":1,"harness":"claude","version":"2.1.100"}`,
	} {
		if err := os.WriteFile(filepath.Join(versions, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.snapshots["claude"]["2.1.318"].SideRequests; got == nil || len(got) != 0 {
		t.Errorf("captured build's side_requests = %#v, want an empty list", got)
	}
	if got := p.snapshots["claude"]["2.1.100"].SideRequests; got != nil {
		t.Errorf("history-only build's side_requests = %#v, want not observed", got)
	}
}

// A platform a build's files were traced on is one it was captured on.
func TestPlatformsIncludeTraces(t *testing.T) {
	s := &Snapshot{Platform: "darwin/arm64", Filesystem: map[string]any{"linux/amd64": map[string]any{}, "darwin/arm64": map[string]any{}}}
	if got := platformsOf(s); len(got) != 2 || got[0] != "darwin/arm64" || got[1] != "linux/amd64" {
		t.Errorf("platforms = %v, want darwin/arm64 and linux/amd64", got)
	}
}
