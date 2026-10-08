package keystore

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

const testKey = "ter_srv_0123456789abcdef"

func keysFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(path(dir))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// keys.json indexes the projects and their hosts; the keys themselves are in the keychain.
func TestKeysLiveInTheKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := SetFor(dir, "claude", "proj-1", testKey, Hosts{OTLP: "https://otlp.example"}); err != nil {
		t.Fatal(err)
	}
	if f := keysFile(t, dir); strings.Contains(f, "ter_srv_") || !strings.Contains(f, "proj-1") {
		t.Fatalf("keys.json must index the key without holding it:\n%s", f)
	}
	if got, err := Get(dir, "proj-1"); err != nil || got != testKey {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if got, err := GetFor(dir, "claude", "proj-1"); err != nil || got != testKey {
		t.Fatalf("GetFor = %q, %v", got, err)
	}
	if got := CollectionProjects(dir); !slices.Equal(got, []string{"proj-1"}) {
		t.Fatalf("CollectionProjects = %v", got)
	}
	if h, ok := HostsFor(dir, "proj-1"); !ok || h.OTLP != "https://otlp.example" {
		t.Fatalf("HostsFor = %+v, %v", h, ok)
	}
}

func TestKeysWithoutAKeychainGoToTheFile(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(t *testing.T, dir string){
		"no keychain": func(t *testing.T, dir string) { secret.FailForTest(t, dir) },
		"insecure storage": func(t *testing.T, dir string) {
			if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			prepare(t, dir)
			if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(keysFile(t, dir), testKey) {
				t.Fatal("the key must fall back to keys.json")
			}
			if got, err := Get(dir, "proj-1"); err != nil || got != testKey {
				t.Fatalf("Get = %q, %v", got, err)
			}
		})
	}
}

// A key the keychain holds but will not give up now is an error, never "no key", so the
// relay neither mints another nor drops what waits for it.
func TestALockedKeychainIsNotAMissingKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := SetFor(dir, "claude", "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, dir)
	if _, err := Get(dir, "proj-1"); !secret.IsUnavailable(err) {
		t.Fatalf("Get = %v, want the keychain unavailable", err)
	}
	if _, err := GetFor(dir, "claude", "proj-1"); !secret.IsUnavailable(err) {
		t.Fatalf("GetFor = %v, want the keychain unavailable", err)
	}
	if got, err := Get(dir, "proj-2"); err != nil || got != "" {
		t.Fatalf("a project with no key = %q, %v; want none, without asking the keychain", got, err)
	}
}

// A key moved to the file by --insecure-storage takes its keychain item along.
func TestInsecureStorageTakesTheKeyItemAlong(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, projectItem("proj-1")); err == nil {
		t.Fatal("the keychain item outlived the move")
	}
	if got, err := Get(dir, "proj-1"); err != nil || got != testKey {
		t.Fatalf("Get = %q, %v", got, err)
	}
}

// Relocate moves every key to where secrets now live: into the keychain, and back into
// keys.json under insecure storage, harness keys included.
func TestRelocateMovesEveryKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock := secret.FailForTest(t, dir)
	if err := SetFor(dir, "claude", "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if !strings.Contains(keysFile(t, dir), testKey) {
		t.Fatal("the key did not fall back to keys.json")
	}
	if err := Relocate(dir); err != nil {
		t.Fatal(err)
	}
	if f := keysFile(t, dir); strings.Contains(f, testKey) {
		t.Fatalf("Relocate left a key in keys.json:\n%s", f)
	}
	if got, err := GetFor(dir, "claude", "proj-1"); err != nil || got != testKey {
		t.Fatalf("GetFor = %q, %v", got, err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := Relocate(dir); err != nil {
		t.Fatal(err)
	}
	if strings.Count(keysFile(t, dir), testKey) != 2 {
		t.Fatalf("insecure storage must bring both keys back to keys.json:\n%s", keysFile(t, dir))
	}
	if _, err := secret.Get(dir, projectItem("proj-1")); err == nil {
		t.Fatal("the keychain item outlived the move")
	}
}

func TestRelocateWithNoKeysWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Relocate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path(dir)); !os.IsNotExist(err) {
		t.Fatalf("Relocate created keys.json: %v", err)
	}
}

// A key whose keychain write timed out lands in keys.json, and the item the write may yet
// fill is marked and removed by the next write, so no untracked copy of a key remains.
func TestATimedOutKeyWriteIsCleanedUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock := secret.TimeOutForTest(t, dir)
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if f := keysFile(t, dir); !strings.Contains(f, testKey) || !strings.Contains(f, `"key/proj-1"`) {
		t.Fatalf("a timed-out write must leave the key in keys.json, its item marked:\n%s", f)
	}
	unlock()
	// The abandoned write lands late.
	if err := secret.Set(dir, projectItem("proj-1"), testKey); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, projectItem("proj-1")); err == nil {
		t.Fatal("the late write's copy outlived the next write")
	}
	if strings.Contains(keysFile(t, dir), "stale_keychain_items") {
		t.Fatal("the mark outlived the item")
	}
}

// DeleteHarnessKeys forgets a project's harness keys and their keychain items, and keeps
// the project's own key and every other project's.
func TestDeleteHarnessKeysLeavesTheProjectsKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, p := range []string{"proj-1", "proj-2"} {
		if err := SetFor(dir, "claude", p, testKey, Hosts{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteHarnessKeys(dir, "proj-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := GetFor(dir, "claude", "proj-1"); err != nil || got != "" {
		t.Fatalf("GetFor = %q, %v; want none", got, err)
	}
	if _, err := secret.Get(dir, harnessItem("claude", "proj-1")); err == nil {
		t.Fatal("the harness key's keychain item outlived its entry")
	}
	if got, err := Get(dir, "proj-1"); err != nil || got != testKey {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if got, err := GetFor(dir, "claude", "proj-2"); err != nil || got != testKey {
		t.Fatalf("another project's harness key = %q, %v", got, err)
	}
}
