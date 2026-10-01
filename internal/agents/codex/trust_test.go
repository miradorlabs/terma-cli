package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
)

// committedRepo is a repository with terma's Codex hooks committed, and a Codex home with
// no trust records yet.
func committedRepo(t *testing.T) (repo, codexHome string) {
	t.Helper()
	repo = hookruntest.InitRepo(t)
	codexHome = t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	plan, err := Agent{}.Plan(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(repo, plan); err != nil {
		t.Fatal(err)
	}
	return repo, codexHome
}

// trustEntries writes the trust record Codex keeps once the developer trusts the kept
// entries, with each entry's hash as Codex computes it.
func trustEntries(t *testing.T, repo, codexHome string, keep func(Entry) bool) {
	t.Helper()
	entries, err := TermaEntries(repo)
	if err != nil || len(entries) == 0 {
		t.Fatalf("terma's entries: %v (%d)", err, len(entries))
	}
	hooksPath := filepath.Join(repo, ".codex", "hooks.json")
	var config strings.Builder
	for _, e := range entries {
		if keep(e) {
			config.WriteString("[hooks.state.\"" + hooksPath + ":" + e.Key() + "\"]\ntrusted_hash = \"" + e.Hash + "\"\nenabled = true\n\n")
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrustAcceptsEveryEntryTrusted(t *testing.T) {
	repo, home := committedRepo(t)
	trustEntries(t, repo, home, func(Entry) bool { return true })
	if st, err := (Agent{}).Trust(repo); err != nil || !st.Trusted {
		t.Fatalf("every entry trusted: %+v, %v", st, err)
	}
}

// A hook that changed after the developer trusted it is not trusted: Codex keys trust on
// the entry's hash.
func TestTrustRejectsAnEntryChangedAfterTrust(t *testing.T) {
	repo, home := committedRepo(t)
	trustEntries(t, repo, home, func(Entry) bool { return true })
	path := filepath.Join(home, "config.toml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := TermaEntries(repo)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, e := range entries {
		if e.Event == "PostToolUse" {
			hash = e.Hash
		}
	}
	after := strings.Replace(string(before), hash, "sha256:stale", 1)
	if hash == "" || after == string(before) {
		t.Fatal("PostToolUse trust hash was not found")
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := (Agent{}).Trust(repo)
	if err != nil || st.Trusted || !strings.Contains(st.Detail, "PostToolUse") || !strings.Contains(st.Fix, "include any new or changed entries") {
		t.Fatalf("a changed entry passed: %+v, %v", st, err)
	}
}

// Trust is per entry: entries added after the developer trusted the file are skipped in
// silence, and Trust names them.
func TestTrustNamesTheEntriesANewerTermaAdded(t *testing.T) {
	repo, home := committedRepo(t)
	trustEntries(t, repo, home, func(e Entry) bool { return !strings.HasPrefix(e.Event, "Subagent") })
	st, err := (Agent{}).Trust(repo)
	if err != nil || st.Trusted {
		t.Fatalf("a file with untrusted entries passed: %+v, %v", st, err)
	}
	for _, want := range []string{"SubagentStart", "SubagentStop"} {
		if !strings.Contains(st.Detail, want) {
			t.Errorf("Trust does not name %s: %+v", want, st)
		}
	}
	if !strings.Contains(st.Fix, "Settings → Hooks → Review") {
		t.Errorf("fix = %q", st.Fix)
	}
}
