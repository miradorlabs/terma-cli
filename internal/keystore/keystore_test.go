package keystore

import (
	"path/filepath"
	"testing"
)

func TestSetForRemembersPerHarnessAndForTheSpool(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	const key = "ter_srv_0123456789abcdef"
	if err := SetFor("claude", "proj-1", key); err != nil {
		t.Fatal(err)
	}
	if got := GetFor("claude", "proj-1"); got != key {
		t.Fatalf("GetFor = %q", got)
	}
	if got := GetFor("codex", "proj-1"); got != "" {
		t.Fatalf("another harness must not inherit the key: %q", got)
	}
	// The spool delivers with whatever key the project has.
	if got := Get("proj-1"); got != key {
		t.Fatalf("Get = %q", got)
	}
	if err := SetFor("", "proj-1", key); err == nil {
		t.Fatal("a harness name is required")
	}
	if err := SetFor("claude", "proj-1", "not-a-key"); err == nil {
		t.Fatal("only server keys are stored")
	}
}

// The existing test hands terma a config directory that already exists, which is why
// nothing noticed that a first key written into a fresh one failed with ENOENT.
func TestSetCreatesTheConfigDirectory(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(t.TempDir(), "not", "there", "yet"))
	const key = "ter_srv_0123456789abcdef"
	if err := Set("proj-1", key); err != nil {
		t.Fatalf("the first key on a new machine: %v", err)
	}
	if got := Get("proj-1"); got != key {
		t.Fatalf("Get = %q", got)
	}
}
