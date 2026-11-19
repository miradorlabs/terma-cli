package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func sampleCredential() *Credential {
	return &Credential{
		AccessToken:    "ter_cli_x",
		OrganizationID: "org-test",
		RefreshToken:   "ter_clr_x",
		ExpiresAt:      time.Now().Add(time.Hour),
	}
}

func orgCredential(org, session string) *Credential {
	return &Credential{
		AccessToken:    "ter_cli_" + org,
		RefreshToken:   "ter_clr_" + org,
		ExpiresAt:      time.Now().Add(time.Hour),
		SessionID:      session,
		OrganizationID: org,
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

	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("stat credentials.json: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.json mode = %o, want 600", perm)
	}
}

func TestMutateCredentialFile_PreservesOtherProfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := SaveCredential(dir, "work", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(dir, work): %v", err)
	}
	if _, err := SaveCredential(dir, "personal", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(dir, personal): %v", err)
	}

	if _, err := LoadCredential(dir, "work"); err != nil {
		t.Errorf("work profile was lost after writing personal: %v", err)
	}
	if _, err := LoadCredential(dir, "personal"); err != nil {
		t.Errorf("personal profile not saved: %v", err)
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

// Tx.DeleteFor removes a credential only at the version it names: one rotated since stays.
// The version it names goes, and no other organization becomes active in its place. Its
// keychain item stays for the next sign-in to overwrite: nothing reads it once unindexed.
func TestTx_DeleteForRemovesOnlyTheVersionItNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, org := range []string{"org-b", "org-a"} {
		if _, err := SaveCredential(dir, "default", orgCredential(org, "s-"+org)); err != nil {
			t.Fatal(err)
		}
	}
	deleteFor := func(refreshToken string) {
		t.Helper()
		if err := lockedWrite(dir, func(tx *Tx) error { return tx.DeleteFor("default", "org-a", refreshToken) }); err != nil {
			t.Fatalf("DeleteFor(%q): %v", refreshToken, err)
		}
	}

	deleteFor("ter_clr_spent")
	if cred, err := LoadCredentialFor(dir, "default", "org-a"); err != nil || cred.RefreshToken != "ter_clr_org-a" {
		t.Fatalf("after deleting another version: %+v, %v; want the credential kept", cred, err)
	}

	deleteFor("ter_clr_org-a")
	if _, err := LoadCredentialFor(dir, "default", "org-a"); err != ErrNotLoggedIn {
		t.Errorf("org-a after deleting its version: %v, want it gone", err)
	}
	if _, err := secret.Get(dir, itemName("default", "org-a")); err != nil {
		t.Errorf("org-a's keychain item: %v, want it left", err)
	}
	if active, err := LoadCredential(dir, "default"); err != ErrNotLoggedIn {
		t.Errorf("active = %+v, %v; want signed out, not moved to org-b", active, err)
	}
	if _, err := LoadCredentialFor(dir, "default", "org-b"); err != nil {
		t.Errorf("org-b: %v, want it kept", err)
	}
	signedIn := orgCredential("org-a", "s-new")
	signedIn.AccessToken, signedIn.RefreshToken = "ter_cli_new", "ter_clr_new"
	if _, err := SaveCredential(dir, "default", signedIn); err != nil {
		t.Fatal(err)
	}
	if cred, err := LoadCredentialFor(dir, "default", "org-a"); err != nil || cred.RefreshToken != "ter_clr_new" {
		t.Errorf("org-a after a new sign-in: %+v, %v; want its tokens", cred, err)
	}
}

// Tx.MarkUnsettled marks only the version it names, keeps the first mark, and the mark
// lives in credentials.json beside the keychain's tokens until a new pair replaces it.
func TestTx_MarkUnsettledMarksOnlyTheVersionItNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, "default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	mark := func(refreshToken string, at time.Time) {
		t.Helper()
		if err := lockedWrite(dir, func(tx *Tx) error { return tx.MarkUnsettled("default", "org-a", refreshToken, at) }); err != nil {
			t.Fatalf("MarkUnsettled(%q): %v", refreshToken, err)
		}
	}
	marked := func() time.Time {
		t.Helper()
		cred, err := LoadCredentialFor(dir, "default", "org-a")
		if err != nil {
			t.Fatal(err)
		}
		return cred.UnsettledSince()
	}
	first := time.Now().Round(0).Add(-time.Minute).Truncate(time.Second)

	mark("ter_clr_rotated", first)
	if got := marked(); !got.IsZero() {
		t.Fatalf("marked %v for another version; want no mark", got)
	}
	mark("ter_clr_org-a", first)
	mark("ter_clr_org-a", first.Add(30*time.Second))
	if got := marked(); !got.Equal(first) {
		t.Fatalf("mark = %v, want the first, %v", got, first)
	}
	earlier := first.Add(-10 * time.Second)
	mark("ter_clr_org-a", earlier)
	if got := marked(); !got.Equal(earlier) {
		t.Fatalf("mark = %v, want the earlier %v", got, earlier)
	}
	if err := lockedWrite(dir, func(tx *Tx) error { return tx.ClearUnsettled("default", "org-a", "ter_clr_org-a", first) }); err != nil {
		t.Fatal(err)
	}
	if got := marked(); !got.Equal(earlier) {
		t.Fatalf("mark = %v after clearing another mark; want %v kept", got, earlier)
	}
	if err := lockedWrite(dir, func(tx *Tx) error { return tx.ExpireUnsettled("default", "org-a", "ter_clr_org-a") }); err != nil {
		t.Fatal(err)
	}
	mark("ter_clr_org-a", time.Now())
	if got := marked(); !got.Equal(UnsettledExpired) {
		t.Fatalf("mark = %v after a later mark; want it expired for good", got)
	}
	if err := UpdateCredential(dir, "default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if got := marked(); !got.IsZero() {
		t.Errorf("mark = %v after a new pair; want none", got)
	}
}

// A mark counts only beside the refresh token it is about. A save whose keychain write
// landed and whose credentials.json write did not leaves it beside another.
func TestCredential_AMarkCountsOnlyForItsToken(t *testing.T) {
	t.Parallel()
	at := time.Now().Round(0)
	cred := &Credential{RefreshToken: "ter_clr_a", RefreshUnsettledAt: at, RefreshUnsettledFor: TokenSHA256("ter_clr_a")}
	if got := cred.UnsettledSince(); !got.Equal(at) {
		t.Errorf("UnsettledSince = %v, want %v", got, at)
	}
	cred.RefreshToken = "ter_clr_b"
	if got := cred.UnsettledSince(); !got.IsZero() {
		t.Errorf("UnsettledSince beside another token = %v, want none", got)
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

func TestSaveCredential_KeepsOneCredentialPerOrganization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := SaveCredential(dir, "default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	replaced, err := SaveCredential(dir, "default", orgCredential("org-b", "s-b"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced != nil {
		t.Fatalf("signing into a second organization replaced nothing, got %+v", replaced)
	}

	active, err := LoadCredential(dir, "default")
	if err != nil || active.OrganizationID != "org-b" {
		t.Fatalf("active = %+v, %v; want org-b", active, err)
	}
	parked, err := LoadCredentialFor(dir, "default", "org-a")
	if err != nil || parked.SessionID != "s-a" {
		t.Fatalf("org-a credential should be kept: %+v, %v", parked, err)
	}
	all, err := Credentials(dir, "default")
	if err != nil || len(all) != 2 || all[0].OrganizationID != "org-b" {
		t.Fatalf("Credentials should list both, active first: %+v, %v", all, err)
	}

	replaced, err = SaveCredential(dir, "default", orgCredential("org-a", "s-a2"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.SessionID != "s-a" {
		t.Fatalf("want the displaced org-a session reported, got %+v", replaced)
	}
}

// A refresh of a parked credential persists it without changing the active organization.
func TestUpdateCredential_DoesNotChangeTheActiveOrganization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, "default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, "default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}

	rotated := orgCredential("org-a", "s-a")
	rotated.AccessToken = "ter_cli_rotated"
	if err := UpdateCredential(dir, "default", rotated); err != nil {
		t.Fatal(err)
	}
	if active, _ := LoadCredential(dir, "default"); active.OrganizationID != "org-b" {
		t.Fatalf("refresh switched the active organization: %+v", active)
	}
	if parked, _ := LoadCredentialFor(dir, "default", "org-a"); parked.AccessToken != "ter_cli_rotated" {
		t.Fatalf("rotated token not persisted: %+v", parked)
	}
}

func TestDeleteCredentialFor_FallsBackToAnotherOrganization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, "default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential(dir, "default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}

	if err := DeleteCredentialFor(dir, "default", "org-b"); err != nil {
		t.Fatal(err)
	}
	active, err := LoadCredential(dir, "default")
	if err != nil || active.OrganizationID != "org-a" {
		t.Fatalf("deleting the active credential should fall back to org-a, got %+v, %v", active, err)
	}
	if err := DeleteCredentialFor(dir, "default", "org-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir, "default"); err != ErrNotLoggedIn {
		t.Fatalf("nothing left should read as not logged in, got %v", err)
	}
}

// An unsupported credential layout must not silently authenticate a profile.
func TestLoadCredentialIgnoresSingleCredentialShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := `{"default":{"access_token":"ter_cli_old","organization_id":"org-a"}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(dir, "default"); err != ErrNotLoggedIn {
		t.Fatalf("unsupported credential authenticated: %v", err)
	}
	if _, err := LoadCredentialFor(dir, "default", "org-a"); err != ErrNotLoggedIn {
		t.Fatalf("unsupported organization authenticated: %v", err)
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
