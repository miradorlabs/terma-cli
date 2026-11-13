package auth

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// legacySignIns is a machine before the migration: two profiles, one signed into two
// organizations.
func legacySignIns(t *testing.T, dir string) {
	t.Helper()
	for _, s := range []struct{ profile, org string }{{"default", "org-b"}, {"default", "org-a"}, {"work", "org-w"}} {
		_, err := SaveCredential(dir, s.profile, orgCredential(s.org, "s-"+s.org))
		mustDo(t, err)
	}
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	mustDo(t, err)
	return string(b)
}

// The copy moves every sign-in, with its tokens, to credentials.v2.json and the v2 items,
// marks the machine migrated, and leaves v1 as it was; every function then reads v2.
func TestCopyToV2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacySignIns(t, dir)
	v1Before := fileText(t, config.CredentialsPath(dir))
	if config.Migrated(dir) || indexOf(dir) != v1 {
		t.Fatal("a machine with only credentials.json is migrated")
	}
	mustDo(t, CopyToV2(dir))
	if !config.Migrated(dir) || indexOf(dir) != v2 {
		t.Fatal("the copy did not mark the machine migrated")
	}
	if fileText(t, config.CredentialsPath(dir)) != v1Before {
		t.Fatal("the copy changed credentials.json")
	}
	if v, err := secret.Get(dir, itemName("default", "org-a")); err != nil || !strings.Contains(v, "ter_cli_org-a") {
		t.Fatalf("the copy took v1's item: %q, %v", v, err)
	}
	if v, err := secret.Get(dir, "credential.v2/default/org-a"); err != nil || !strings.Contains(v, "ter_cli_org-a") {
		t.Fatalf("v2's item = %q, %v", v, err)
	}
	if f := fileText(t, config.CredentialsV2Path(dir)); strings.Contains(f, "ter_cli_") || !strings.Contains(f, "org-w") {
		t.Fatalf("credentials.v2.json must index every sign-in without its tokens:\n%s", f)
	}
	cred, err := LoadCredential(dir, "default")
	if err != nil || cred.OrganizationID != "org-a" || cred.AccessToken != "ter_cli_org-a" {
		t.Fatalf("LoadCredential = %+v, %v; want the active org-a", cred, err)
	}
	if creds, err := Credentials(dir, "default"); err != nil || len(creds) != 2 {
		t.Fatalf("Credentials = %d, %v", len(creds), err)
	}
	if _, err := LoadCredentialFor(dir, "work", "org-w"); err != nil {
		t.Fatal(err)
	}

	// A refresh after the copy writes v2 alone, under v2's own lock.
	rotated := orgCredential("org-a", "s-org-a")
	rotated.AccessToken, rotated.RefreshToken = "ter_cli_rotated", "ter_clr_rotated"
	mustDo(t, UpdateCredential(dir, "default", rotated))
	if _, err := os.Stat(config.CredentialsV2Path(dir) + ".lock"); err != nil {
		t.Fatalf("a v2 write took no lock of its own: %v", err)
	}
	if v, _ := secret.Get(dir, itemName("default", "org-a")); strings.Contains(v, "rotated") {
		t.Fatal("a refresh after the copy wrote v1's item")
	}
	// Copying again never puts the spent pair back.
	mustDo(t, CopyToV2(dir))
	if cred, _ := LoadCredentialFor(dir, "default", "org-a"); cred.RefreshToken != "ter_clr_rotated" {
		t.Fatalf("a repeated copy overwrote the rotated pair: %+v", cred)
	}
}

// A machine with no sign-ins is migrated by its copy all the same: credentials.v2.json is
// the marker that outlives a lost routing state.
func TestCopyToV2MarksAnEmptyMachine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustDo(t, CopyToV2(dir))
	if !config.Migrated(dir) {
		t.Fatal("an empty machine was not marked migrated")
	}
	if _, err := LoadCredential(dir, "default"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("LoadCredential = %v", err)
	}
	mustDo(t, RetireV1(dir))
}

// A crash between the copy and the retirement: the next start copies again, which changes
// nothing, then retires v1, whose sign-ins and items go while v2's stay.
func TestCrashBetweenCopyAndRetire(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacySignIns(t, dir)
	if err := RetireV1(dir); err == nil {
		t.Fatal("retired sign-ins credentials.v2.json does not hold")
	}
	if _, err := loadCredentials(dir, v1); err != nil || !strings.Contains(fileText(t, config.CredentialsPath(dir)), "org-a") {
		t.Fatal("a refused retirement changed credentials.json")
	}
	mustDo(t, CopyToV2(dir))
	v2Before := fileText(t, config.CredentialsV2Path(dir))
	// The relay stops here. credentials.json still holds every sign-in.
	mustDo(t, CopyToV2(dir))
	if fileText(t, config.CredentialsV2Path(dir)) != v2Before {
		t.Fatal("repeating the copy changed credentials.v2.json")
	}
	mustDo(t, RetireV1(dir))
	old, err := loadCredentials(dir, v1)
	mustDo(t, err)
	for profile, p := range old {
		if p.active() != nil || len(p.Organizations) != 0 {
			t.Errorf("credentials.json still holds %s's sign-ins: %+v", profile, p)
		}
	}
	for _, item := range []string{itemName("default", "org-a"), itemName("default", "org-b"), itemName("work", "org-w")} {
		if _, err := secret.Get(dir, item); !errors.Is(err, secret.ErrNotFound) {
			t.Errorf("%s survived the retirement: %v", item, err)
		}
	}
	if cred, err := LoadCredentialFor(dir, "default", "org-b"); err != nil || cred.AccessToken != "ter_cli_org-b" {
		t.Fatalf("v2 lost a sign-in: %+v, %v", cred, err)
	}
	mustDo(t, RetireV1(dir))
	mustDo(t, CopyToV2(dir))

	// Signing out of a migrated machine removes v2's items.
	mustDo(t, DeleteCredential(dir, "work"))
	if _, err := secret.Get(dir, "credential.v2/work/org-w"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("sign-out left v2's item: %v", err)
	}
}

// A keychain that will not remove a legacy item fails the retirement with credentials.json
// as it was, and retiring again finishes it.
func TestRetireV1KeepsTheFileUntilItsItemsAreGone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacySignIns(t, dir)
	mustDo(t, CopyToV2(dir))
	before := fileText(t, config.CredentialsPath(dir))
	unlock := secret.FailForTest(t, dir)
	if err := RetireV1(dir); err == nil {
		t.Fatal("retired with items the keychain kept")
	}
	if fileText(t, config.CredentialsPath(dir)) != before {
		t.Fatal("a failed retirement changed credentials.json")
	}
	unlock()
	mustDo(t, RetireV1(dir))
	if _, err := secret.Get(dir, itemName("default", "org-a")); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the item survived: %v", err)
	}
}

// A legacy sign-in whose tokens are gone is copied as it reads, signed out; one kept in
// the file under insecure storage stays in the file in v2.
func TestCopyToV2KeepsHowEachSignInReads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacySignIns(t, dir)
	mustDo(t, secret.Delete(dir, itemName("default", "org-b")))
	mustDo(t, config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }))
	mustDo(t, CopyToV2(dir))
	if _, err := LoadCredentialFor(dir, "default", "org-b"); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("a signed-out legacy sign-in reads as %v", err)
	}
	if !strings.Contains(fileText(t, config.CredentialsV2Path(dir)), "ter_cli_org-a") || !StoredInFile(dir, "default") {
		t.Fatal("under insecure storage the copy must keep the tokens in credentials.v2.json")
	}
	mustDo(t, RetireV1(dir))
}
