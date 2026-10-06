//go:build unix

package auth

import (
	"os"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Moving tokens to the file deletes their keychain item only once the file is on disk:
// a save that fails leaves the item, the only durable copy, where it was.
func TestAFailedSaveKeepsTheKeychainItem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, orgCredential("org-a", "s1")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := SaveCredential(dir, config.DefaultProfile, orgCredential("org-a", "s1")); err == nil {
		t.Fatal("the save into a read-only directory succeeded")
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile, "org-a")); err != nil {
		t.Fatalf("the keychain item went before the file held its tokens: %v", err)
	}
}

// Tokens the keychain took for a new organization, whose index the file then failed to
// save, are taken back out of the keychain: nothing could otherwise find them.
func TestAFailedSaveTakesBackANewCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, orgCredential("org-a", "s1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := SaveCredential(dir, config.DefaultProfile, orgCredential("org-b", "s2")); err == nil {
		t.Fatal("the save into a read-only directory succeeded")
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile, "org-b")); err == nil {
		t.Fatal("tokens no file indexes were left in the keychain")
	}
}

// A relocation into the keychain whose file save fails takes the copies back: the file
// still holds the tokens, and nothing would track the keychain's.
func TestAFailedRelocationTakesBackItsCopies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, config.DefaultProfile, orgCredential("org-a", "s1")); err != nil {
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
	if _, err := secret.Get(dir, itemName(config.DefaultProfile, "org-a")); err == nil {
		t.Fatal("a keychain copy no file indexes was left behind")
	}
}
