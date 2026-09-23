package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// codexHomeWith writes a user config.toml holding body and points CODEX_HOME at it.
func codexHomeWith(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexHookTrustUnreviewed(t *testing.T) {
	// A fresh machine: Codex has never been shown this repository's hooks, so it runs
	// none of them, and nothing in the config says so.
	codexHomeWith(t, "model = \"gpt-6\"\n")
	trust, err := (Codex{}).CodexHookTrustFor("/repo/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if trust.Reviewed() || trust.Entries != 0 || trust.Trusted != 0 {
		t.Fatalf("want no record at all, got %+v", trust)
	}
}

func TestCodexHookTrustCountsOnlyThisFile(t *testing.T) {
	codexHomeWith(t, `
[hooks.state."/repo/.codex/hooks.json:session_start:0:0"]
trusted_hash = "sha256:aaa"
enabled = true

[hooks.state."/repo/.codex/hooks.json:post_tool_use:0:0"]
trusted_hash = "sha256:bbb"

[hooks.state."/repo/.codex/hooks.json:session_end:0:0"]
enabled = false
trusted_hash = "sha256:ccc"

[hooks.state."/Users/dev/.codex/hooks.json:stop:0:0"]
trusted_hash = "sha256:ddd"
`)
	trust, err := (Codex{}).CodexHookTrustFor("/repo/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if !trust.Reviewed() {
		t.Fatal("want the file reported as reviewed")
	}
	// The user's own hooks file must not be counted against the repository's.
	if trust.Entries != 3 {
		t.Fatalf("Entries = %d, want 3", trust.Entries)
	}
	if trust.Trusted != 3 {
		t.Fatalf("Trusted = %d, want 3", trust.Trusted)
	}
	if trust.Disabled != 1 {
		t.Fatalf("Disabled = %d, want 1", trust.Disabled)
	}
}

func TestCodexHookTrustSeenButNotTrusted(t *testing.T) {
	// Codex records an entry the moment it discovers a hook; without a hash it is
	// still waiting for review and will not run.
	codexHomeWith(t, `
[hooks.state."/repo/.codex/hooks.json:session_start:0:0"]
enabled = true
`)
	trust, err := (Codex{}).CodexHookTrustFor("/repo/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if !trust.Reviewed() {
		t.Fatal("an entry exists, so the file has been seen")
	}
	if trust.Trusted != 0 {
		t.Fatalf("Trusted = %d, want 0 without a hash", trust.Trusted)
	}
}

func TestCodexHookTrustWithoutAConfig(t *testing.T) {
	// No config.toml at all is the ordinary state of a machine that has run Codex
	// once and never configured it; it must read as "not trusted", not as an error.
	codexHomeWith(t, "")
	trust, err := (Codex{}).CodexHookTrustFor("/repo/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	if trust.Reviewed() {
		t.Fatalf("want no record, got %+v", trust)
	}
}
