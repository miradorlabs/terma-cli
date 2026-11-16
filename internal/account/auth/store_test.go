package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

// activeIn is the organization the profile's index in path marks active.
func activeIn(t *testing.T, path, profile string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file credentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file[profile] == nil {
		return ""
	}
	return file[profile].Active
}

// A sign-in to several organizations saves each without activating it: the active
// organization never changes, a profile with none keeps none, and each save returns the
// session it displaced in its own organization only. ActivateCredential then makes one
// active, and only one the profile holds. On a machine not migrated it all lands in
// credentials.json; on a migrated one in credentials.v2.json alone.
func TestSaveInactiveCredential(t *testing.T) {
	t.Parallel()
	for _, migrated := range []bool{false, true} {
		dir := t.TempDir()
		if migrated {
			mustDo(t, os.WriteFile(config.CredentialsV2Path(dir), []byte("{}"), 0o600))
		}
		index := config.CredentialsPath(dir)
		if migrated {
			index = config.CredentialsV2Path(dir)
		}

		replaced, err := SaveInactiveCredential(dir, "default", orgCredential("org-p", "s-p"))
		mustDo(t, err)
		if replaced != nil || activeIn(t, index, "default") != "" {
			t.Fatalf("migrated %v: a first inactive save replaced %+v and activated %q", migrated, replaced, activeIn(t, index, "default"))
		}
		if _, err := LoadCredential(dir, "default"); err != ErrNotLoggedIn {
			t.Fatalf("migrated %v: a profile with no active organization reads as %v", migrated, err)
		}
		if cred, err := LoadCredentialFor(dir, "default", "org-p"); err != nil || cred.AccessToken != "ter_cli_org-p" {
			t.Fatalf("migrated %v: the inactive sign-in = %+v, %v", migrated, cred, err)
		}

		_, err = SaveCredential(dir, "default", orgCredential("org-a", "s-a"))
		mustDo(t, err)
		for _, org := range []string{"org-b", "org-p"} {
			replaced, err := SaveInactiveCredential(dir, "default", orgCredential(org, "s-"+org+"-2"))
			mustDo(t, err)
			if active := activeIn(t, index, "default"); active != "org-a" {
				t.Fatalf("migrated %v: saving %s made %q active", migrated, org, active)
			}
			switch {
			case org == "org-p" && (replaced == nil || replaced.SessionID != "s-p"):
				t.Fatalf("migrated %v: org-p's displaced session = %+v, want s-p", migrated, replaced)
			case org == "org-b" && replaced != nil:
				t.Fatalf("migrated %v: a new organization displaced %+v", migrated, replaced)
			}
		}
		// Saving the same session again displaces nothing to revoke.
		if replaced, err := SaveInactiveCredential(dir, "default", orgCredential("org-b", "s-org-b-2")); err != nil || replaced != nil {
			t.Fatalf("migrated %v: re-saving a session replaced %+v, %v", migrated, replaced, err)
		}

		if err := ActivateCredential(dir, "default", "org-c"); err != ErrNotLoggedIn {
			t.Fatalf("migrated %v: activating an organization without a sign-in = %v", migrated, err)
		}
		mustDo(t, ActivateCredential(dir, "default", "org-p"))
		if active, err := LoadCredential(dir, "default"); err != nil || active.OrganizationID != "org-p" || active.SessionID != "s-org-p-2" {
			t.Fatalf("migrated %v: active after activating org-p = %+v, %v", migrated, active, err)
		}
		if creds, err := Credentials(dir, "default"); err != nil || len(creds) != 3 {
			t.Fatalf("migrated %v: credentials = %d, %v", migrated, len(creds), err)
		}
		if migrated {
			if _, err := os.Stat(config.CredentialsPath(dir)); err == nil {
				t.Fatal("a migrated machine's inactive save wrote credentials.json")
			}
		} else if config.Migrated(dir) {
			t.Fatal("an inactive save migrated the machine")
		}
	}
}
