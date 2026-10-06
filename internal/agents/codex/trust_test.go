package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// userHooked is a Codex home holding terma's machine-wide hooks and no trust records yet;
// it returns the hooks file.
func userHooked(t *testing.T) (codexHome, hooksFile string) {
	t.Helper()
	return userHookedWith(t, testCommand)
}

// userHookedWith is userHooked with setup's entries running command.
func userHookedWith(t *testing.T, command func(string) string) (codexHome, hooksFile string) {
	t.Helper()
	codexHome, _ = filepath.EvalSymlinks(t.TempDir()) // one spelling, as in Codex's records
	t.Setenv("CODEX_HOME", codexHome)
	plan, err := planUserHooks(codexHome, command, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := hookmgr.Apply(codexHome, plan); err != nil {
		t.Fatal(err)
	}
	return codexHome, filepath.Join(codexHome, "hooks.json")
}

// Machine-wide hooks are present once setup writes them,
// trusted only while every entry's hash is the one Codex recorded.
func TestUserHooksTrustedFollowsTheRecordedHashes(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if present, _, err := (Agent{}).UserHooksTrusted(); err != nil || present {
		t.Fatalf("no hooks file: present %v, %v", present, err)
	}
	write := func(terma string) {
		t.Helper()
		plan, err := planUserHooks(codexHome, hookmgr.UserHookCommand(terma), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := hookmgr.Apply(codexHome, plan); err != nil {
			t.Fatal(err)
		}
	}
	write("/opt/terma/bin/terma")
	if present, trusted, err := (Agent{}).UserHooksTrusted(); err != nil || !present || trusted {
		t.Fatalf("fresh hooks: present %v trusted %v, %v", present, trusted, err)
	}
	hooksFile := filepath.Join(codexHome, "hooks.json")
	entries, err := termaEntriesIn(hooksFile)
	if err != nil || len(entries) == 0 {
		t.Fatalf("terma's entries: %v (%d)", err, len(entries))
	}
	var config strings.Builder
	for _, e := range entries {
		config.WriteString("[hooks.state." + quoteTOMLString(hooksFile+":"+e.Key()) + "]\ntrusted_hash = \"" + e.Hash + "\"\n\n")
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, trusted, err := (Agent{}).UserHooksTrusted(); err != nil || !trusted {
		t.Fatalf("every entry trusted: %v, %v", trusted, err)
	}
	// A setup from another terma path rewrites every entry, and Codex skips them all.
	write("/usr/local/bin/terma")
	if _, trusted, err := (Agent{}).UserHooksTrusted(); err != nil || trusted {
		t.Fatalf("rewritten hooks still trusted: %v, %v", trusted, err)
	}
}
