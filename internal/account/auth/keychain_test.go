package auth

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func credentialsFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(config.CredentialsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// rotated is session's sign-in after a refresh.
func rotated(session string) *Credential {
	c := sessionCredential(session)
	c.AccessToken, c.RefreshToken = "access-rotated", "refresh-rotated"
	return c
}

// Signing in puts the tokens in the keychain and only the index in the file, and a
// refresh keeps them there.
func TestTokensLiveInTheKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	if f := credentialsFile(t, dir); strings.Contains(f, "access-") || strings.Contains(f, "refresh-") || !strings.Contains(f, `"session_id": "s1"`) {
		t.Fatalf("user-credentials.json must index the credential without its tokens:\n%s", f)
	}
	if StoredInFile(dir, config.DefaultProfile) {
		t.Fatal("StoredInFile = true")
	}
	cred, err := LoadCredential(dir, config.DefaultProfile)
	if err != nil || cred.AccessToken != "access-rotated" || cred.RefreshToken != "refresh-rotated" {
		t.Fatalf("LoadCredential = %+v, %v; want the rotated pair", cred, err)
	}
}

func TestInsecureStorageKeepsTokensInTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(credentialsFile(t, dir), "access-s1") || !StoredInFile(dir, config.DefaultProfile) {
		t.Fatal("--insecure-storage must keep the tokens in user-credentials.json")
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the keychain holds an item: %v", err)
	}
}

// With no keychain to use, signing in falls back to the file, as gh does, and later
// refreshes stay there.
func TestNoKeychainFallsBackToTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret.FailForTest(t, dir)
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if !StoredInFile(dir, config.DefaultProfile) {
		t.Fatal("the tokens must fall back to user-credentials.json")
	}
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(dir, config.DefaultProfile)
	if err != nil || cred.AccessToken != "access-rotated" {
		t.Fatalf("LoadCredential = %+v, %v", cred, err)
	}
	if err := DeleteCredential(dir, config.DefaultProfile); err != nil {
		t.Fatalf("signing out of a file-only credential must not need the keychain: %v", err)
	}
}

// A refresh while the keychain is locked writes the rotated pair to the file, which a
// reader prefers, so the rotation is never lost.
func TestARefreshTheKeychainRefusesLandsInTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, dir)
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(dir, config.DefaultProfile)
	if err != nil || cred.AccessToken != "access-rotated" {
		t.Fatalf("LoadCredential = %+v, %v", cred, err)
	}
}

// A refresh the keychain refuses for this one item, while it takes others, lands in the
// file too.
func TestARefreshTheKeychainRefusesForItsItemLandsInTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	secret.RefuseForTest(t, dir, itemName(config.DefaultProfile))
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	if !StoredInFile(dir, config.DefaultProfile) {
		t.Fatal("the refused refresh did not land in the file")
	}
	cred, err := LoadCredential(dir, config.DefaultProfile)
	if err != nil || cred.AccessToken != "access-rotated" {
		t.Fatalf("LoadCredential = %+v, %v", cred, err)
	}
}

// A locked keychain is not a signed-out developer: nothing may read it as ErrNotLoggedIn
// and drop the credential.
func TestALockedKeychainIsNotSignedOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, dir)
	for name, err := range map[string]error{
		"LoadCredential": func() error { _, err := LoadCredential(dir, config.DefaultProfile); return err }(),
		"Tx.Load": Locked(t.Context(), dir, func(tx *Tx) error {
			_, err := tx.Load(config.DefaultProfile)
			return err
		}),
		"DeleteCredentialIf": DeleteCredentialIf(dir, config.DefaultProfile, "refresh-s1"),
	} {
		if !secret.IsUnavailable(err) || errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("%s = %v, want the keychain unavailable", name, err)
		}
	}
	if _, err := LoadIdentity(dir, config.DefaultProfile); err != nil {
		t.Fatalf("the sign-in was dropped: %v", err)
	}
}

func TestSigningOutRemovesTheKeychainItem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, profile := range []string{config.DefaultProfile, "work"} {
		if _, err := SaveCredential(dir, profile, sessionCredential("s-"+profile)); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteCredential(dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the tokens outlived sign-out: %v", err)
	}
	if _, err := secret.Get(dir, itemName("work")); err != nil {
		t.Fatalf("signing out of one profile removed another's tokens: %v", err)
	}
}

// Signing in again returns the session it displaced with its tokens, read before the
// new ones overwrote the item, so the caller can revoke it.
func TestSigningInAgainReturnsTheDisplacedSessionsTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	replaced, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s2"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.SessionID != "s1" || replaced.AccessToken != "access-s1" {
		t.Fatalf("replaced = %+v, want session s1 with its own token", replaced)
	}
}

// Signing in with --insecure-storage moves keychain-held tokens to the file and takes the
// item along, so nothing is left in the keychain for sign-out to miss.
func TestInsecureStorageTakesTheKeychainItemAlong(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the keychain item outlived the move: %v", err)
	}
	if !StoredInFile(dir, config.DefaultProfile) {
		t.Fatal("the tokens did not move to the file")
	}
}

// LoadIdentity answers who is signed in from the file alone: a locked keychain does not
// hide the user.
func TestLoadIdentityNeverReadsTheKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, dir)
	cred, err := LoadIdentity(dir, config.DefaultProfile)
	if err != nil || cred.UserID != "user-1" || cred.SessionID != "s1" || cred.AccessToken != "" {
		t.Fatalf("LoadIdentity = %+v, %v", cred, err)
	}
}

// Moving keychain-held tokens to the file while the keychain is locked marks the item
// left behind, and sign-out removes it once the keychain lets it go.
func TestAnItemTheKeychainKeptIsRemovedAtSignOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	unlock := secret.FailForTest(t, dir)
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(credentialsFile(t, dir), `"stale_keychain_item": true`) {
		t.Fatalf("the item left in the keychain is not marked:\n%s", credentialsFile(t, dir))
	}
	unlock()
	if err := DeleteCredential(dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the keychain item outlived sign-out: %v", err)
	}
}

// A refresh the keychain refuses lands in the file and marks the old item it left; the
// next refresh, with the keychain back, removes that item and clears the mark.
func TestARefusedRefreshMarksTheItemItLeft(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	unlock := secret.FailForTest(t, dir)
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(credentialsFile(t, dir), `"stale_keychain_item": true`) {
		t.Fatal("the refused refresh did not mark the item it left")
	}
	unlock()
	if err := UpdateCredential(dir, config.DefaultProfile, rotated("s1")); err != nil {
		t.Fatal(err)
	}
	if f := credentialsFile(t, dir); strings.Contains(f, "stale_keychain_item") {
		t.Fatalf("the mark outlived the item:\n%s", f)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the stale item is still there: %v", err)
	}
}

// A keychain write that times out may still land after the tokens went to the file: the
// credential is marked, and sign-out removes whatever the write left.
func TestATimedOutWriteIsCleanedUpAtSignOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock := secret.TimeOutForTest(t, dir)
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if !StoredInFile(dir, config.DefaultProfile) || !strings.Contains(credentialsFile(t, dir), `"stale_keychain_item": true`) {
		t.Fatalf("a timed-out write must leave the tokens in the file, marked:\n%s", credentialsFile(t, dir))
	}
	unlock()
	// The abandoned write lands late.
	if err := secret.Set(dir, itemName(config.DefaultProfile), `{"access_token":"late"}`); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCredential(dir, config.DefaultProfile); err != nil {
		t.Fatal(err)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the late write outlived sign-out: %v", err)
	}
}

// With no keychain at all, nothing is marked: signing out of a file-only credential never
// asks the keychain or reports a removal it could not make.
func TestNoKeychainMarksNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret.FailForTest(t, dir)
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(credentialsFile(t, dir), "stale_keychain_item") {
		t.Fatal("a refusal, unlike a timeout, cannot land later and needs no mark")
	}
}

// Relocate moves every profile's tokens: into the file under insecure storage, and back
// into the keychain without it.
func TestRelocateMovesEveryCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	profiles := []string{config.DefaultProfile, "work"}
	for _, profile := range profiles {
		if _, err := SaveCredential(dir, profile, sessionCredential("s-"+profile)); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if err := Relocate(dir); err != nil {
		t.Fatal(err)
	}
	f := credentialsFile(t, dir)
	for _, profile := range profiles {
		if !strings.Contains(f, "access-s-"+profile) {
			t.Fatalf("insecure storage must bring %s's tokens into the file:\n%s", profile, f)
		}
		if _, err := secret.Get(dir, itemName(profile)); !errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("%s's keychain item outlived the move: %v", profile, err)
		}
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = false }); err != nil {
		t.Fatal(err)
	}
	if err := Relocate(dir); err != nil {
		t.Fatal(err)
	}
	if f := credentialsFile(t, dir); strings.Contains(f, "access-") {
		t.Fatalf("Relocate left tokens in the file:\n%s", f)
	}
	for _, profile := range profiles {
		if cred, err := LoadCredential(dir, profile); err != nil || cred.AccessToken != "access-s-"+profile {
			t.Fatalf("LoadCredential(%s) = %+v, %v", profile, cred, err)
		}
	}
}

// A relocation whose keychain write times out keeps the tokens in the file and marks the
// item the write may yet fill, so nothing is left untracked.
func TestADelayedRelocationKeepsItsMark(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateFile(dir, func(f *config.File) { f.InsecureStorage = false }); err != nil {
		t.Fatal(err)
	}
	secret.TimeOutForTest(t, dir)
	_ = Relocate(dir)
	if !StoredInFile(dir, config.DefaultProfile) || !strings.Contains(credentialsFile(t, dir), `"stale_keychain_item": true`) {
		t.Fatalf("a delayed relocation must keep the tokens in the file and mark the item:\n%s", credentialsFile(t, dir))
	}
}
