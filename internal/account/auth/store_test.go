package auth

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func sampleCredential() *Credential {
	return sessionCredential("s1")
}

// sessionCredential is a sign-in of session whose tokens name it.
func sessionCredential(session string) *Credential {
	return &Credential{
		AccessToken:  "access-" + session,
		RefreshToken: "refresh-" + session,
		ExpiresAt:    time.Now().Add(time.Hour),
		SessionID:    session,
		AuthURL:      "https://auth.example.test",
		UserID:       "user-1",
	}
}

func TestSaveCredential_WritesFile0600(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes do not apply on Windows")
	}
	dir := t.TempDir()

	if _, err := SaveCredential(dir, config.DefaultProfile, sampleCredential()); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	info, err := os.Stat(config.CredentialsPath(dir))
	if err != nil {
		t.Fatalf("stat user-credentials.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("user-credentials.json mode = %o, want 600", perm)
	}
}

func TestMutateCredentialFile_PreservesOtherProfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := SaveCredential(dir, "work", sessionCredential("s-work")); err != nil {
		t.Fatalf("SaveCredential(dir, work): %v", err)
	}
	if _, err := SaveCredential(dir, "personal", sessionCredential("s-personal")); err != nil {
		t.Fatalf("SaveCredential(dir, personal): %v", err)
	}

	if cred, err := LoadCredential(dir, "work"); err != nil || cred.AccessToken != "access-s-work" {
		t.Errorf("work profile was lost after writing personal: %+v, %v", cred, err)
	}
	if cred, err := LoadCredential(dir, "personal"); err != nil || cred.AccessToken != "access-s-personal" {
		t.Errorf("personal profile not saved: %+v, %v", cred, err)
	}
}

func TestDeleteCredential_RemovesOnlyTheNamedProfile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := SaveCredential(dir, "work", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(dir, work): %v", err)
	}
	if _, err := SaveCredential(dir, "personal", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(dir, personal): %v", err)
	}

	if err := DeleteCredential(dir, "work"); err != nil {
		t.Fatalf("DeleteCredential(dir, work): %v", err)
	}

	if _, err := LoadCredential(dir, "work"); err != ErrNotLoggedIn {
		t.Errorf("work profile should be gone, got err=%v", err)
	}
	if _, err := LoadCredential(dir, "personal"); err != nil {
		t.Errorf("personal profile should survive deleting work: %v", err)
	}
}

// TestDeleteCredential_MissingProfileIsNoError keeps logout idempotent.
func TestDeleteCredential_MissingProfileIsNoError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := DeleteCredential(dir, "never-existed"); err != nil {
		t.Errorf("deleting a missing profile should be a no-op, got %v", err)
	}
}

// Signing in as a different session returns the one it replaced, for the caller to revoke.
// Signing in again as the same session returns nothing, since there is nothing to revoke.
func TestSaveCredential_ReturnsTheReplacedSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	replaced, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1"))
	if err != nil || replaced != nil {
		t.Fatalf("the first sign-in = %+v, %v; want nothing replaced", replaced, err)
	}
	replaced, err = SaveCredential(dir, config.DefaultProfile, sessionCredential("s1"))
	if err != nil || replaced != nil {
		t.Fatalf("signing in again as the same session = %+v, %v; want nothing replaced", replaced, err)
	}
	replaced, err = SaveCredential(dir, config.DefaultProfile, sessionCredential("s2"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.SessionID != "s1" || replaced.RefreshToken != "refresh-s1" {
		t.Fatalf("replaced = %+v; want session s1 with its tokens", replaced)
	}
	if cred, err := LoadCredential(dir, config.DefaultProfile); err != nil || cred.SessionID != "s2" {
		t.Fatalf("LoadCredential = %+v, %v; want session s2", cred, err)
	}
}

// DeleteCredentialIf forgets the sign-in only while it still holds the caller's refresh
// token: one another process stored since stays.
func TestDeleteCredentialIf_DeletesOnlyTheCallersSignIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}

	if err := DeleteCredentialIf(dir, config.DefaultProfile, "refresh-other"); err != nil {
		t.Fatal(err)
	}
	if cred, err := LoadCredential(dir, config.DefaultProfile); err != nil || cred.SessionID != "s1" {
		t.Fatalf("a sign-in with another refresh token was deleted: %+v, %v", cred, err)
	}

	if err := DeleteCredentialIf(dir, config.DefaultProfile, "refresh-s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir, config.DefaultProfile); err != ErrNotLoggedIn {
		t.Fatalf("the matching sign-in survived: %v", err)
	}
	if _, err := secret.Get(dir, itemName(config.DefaultProfile)); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the keychain item outlived the sign-in: %v", err)
	}

	if err := DeleteCredentialIf(dir, "never-existed", "refresh-s1"); err != nil {
		t.Fatalf("deleting a missing profile should be a no-op, got %v", err)
	}
}

// A writer must not take a sign-in whose keychain item is missing for signed out: Tx.Load
// reports an error, while a reader sees ErrNotLoggedIn.
func TestTxLoad_AMissingKeychainItemIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sampleCredential()); err != nil {
		t.Fatal(err)
	}
	if err := secret.Delete(dir, itemName(config.DefaultProfile)); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadCredential(dir, config.DefaultProfile); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("LoadCredential = %v, want ErrNotLoggedIn", err)
	}
	err := Locked(t.Context(), dir, func(tx *Tx) error {
		_, err := tx.Load(config.DefaultProfile)
		return err
	})
	if err == nil || errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Tx.Load = %v; want an error that is not ErrNotLoggedIn", err)
	}
	if err := DeleteCredentialIf(dir, config.DefaultProfile, "refresh-s1"); err == nil {
		t.Fatal("DeleteCredentialIf deleted a sign-in whose tokens it could not read")
	}
}

// An entry without a session must not authenticate a profile, and an older terma's
// credentials.json does not sign anyone in.
func TestLoadCredential_IgnoresAnEntryWithoutASession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := `{"default":{"access_token":"access-old","auth_url":"https://auth.example.test"}}`
	if err := os.WriteFile(config.CredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := `{"work":{"active":"org-a","organizations":{"org-a":{"access_token":"access-old","session_id":"s-old"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"default", "work"} {
		if _, err := LoadCredential(dir, profile); err != ErrNotLoggedIn {
			t.Fatalf("LoadCredential(%s) = %v; want ErrNotLoggedIn", profile, err)
		}
	}
}

// A write waits for a holder that hangs only until lockWait, then fails naming the lock.
func TestSaveCredential_GivesUpOnAHungHolder(t *testing.T) {
	saved := lockWait
	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = saved })
	dir := t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error)
	go func() {
		done <- Locked(t.Context(), dir, func(*Tx) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	start := time.Now()
	_, err := SaveCredential(dir, config.DefaultProfile, sampleCredential())
	elapsed := time.Since(start)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err == nil || !strings.Contains(err.Error(), "another terma process held them") {
		t.Fatalf("SaveCredential behind a hung holder = %v; want the lock's deadline", err)
	}
	if elapsed > lockWait+2*time.Second {
		t.Fatalf("SaveCredential waited %v; want about %v", elapsed, lockWait)
	}
}
