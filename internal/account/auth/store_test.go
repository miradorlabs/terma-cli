package auth

import (
	"os"
	"path/filepath"
	"runtime"
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
