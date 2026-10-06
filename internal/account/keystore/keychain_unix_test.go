//go:build unix

package keystore

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Moving a key to keys.json deletes its keychain item only once the file is on disk: a
// save that fails leaves the item, the only durable copy, where it was.
func TestAFailedSaveKeepsTheKeyItem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Relocate(dir); err == nil {
		t.Fatal("the save into a read-only directory succeeded")
	}
	if _, err := secret.Get(dir, projectItem("proj-1")); err != nil {
		t.Fatalf("the keychain item went before keys.json held its key: %v", err)
	}
}

// A new key the keychain took, whose index the file then failed to save, is taken back
// out of the keychain: nothing could otherwise find it.
func TestAFailedSaveTakesBackANewKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Set(dir, "proj-2", testKey, Hosts{}); err == nil {
		t.Fatal("the save into a read-only directory succeeded")
	}
	if _, err := secret.Get(dir, projectItem("proj-2")); err == nil {
		t.Fatal("a key no file indexes was left in the keychain")
	}
	if got, err := Get(dir, "proj-1"); err != nil || got != testKey {
		t.Fatalf("the earlier key = %q, %v", got, err)
	}
}

// A relocation into the keychain whose file save fails takes the copies back.
func TestAFailedRelocationTakesBackItsKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "proj-1", testKey, Hosts{}); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = false }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := Relocate(dir); err == nil {
		t.Fatal("the save into a read-only directory succeeded")
	}
	if _, err := secret.Get(dir, projectItem("proj-1")); err == nil {
		t.Fatal("a keychain copy no file indexes was left behind")
	}
}
