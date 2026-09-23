package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func sampleCredential() *Credential {
	return &Credential{
		AccessToken:  "mir_cli_x",
		RefreshToken: "mir_clr_x",
		ExpiresAt:    time.Now().Add(time.Hour),
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
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes do not apply on Windows")
	}
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)

	if _, err := SaveCredential(config.DefaultProfile, sampleCredential()); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("stat credentials.json: %v", err)
	}
	// This file holds live access and refresh tokens; it must never be group- or
	// world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials.json mode = %o, want 600", perm)
	}
}

func TestMutateCredentialFile_PreservesOtherProfiles(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := SaveCredential("work", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(work): %v", err)
	}
	// A second profile's write is a whole-file read-modify-write; without the lock and
	// re-read it would clobber the first profile's entry.
	if _, err := SaveCredential("personal", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(personal): %v", err)
	}

	if _, err := LoadCredential("work"); err != nil {
		t.Errorf("work profile was lost after writing personal: %v", err)
	}
	if _, err := LoadCredential("personal"); err != nil {
		t.Errorf("personal profile not saved: %v", err)
	}
}

func TestDeleteCredential_RemovesOnlyTheNamedProfile(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := SaveCredential("work", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(work): %v", err)
	}
	if _, err := SaveCredential("personal", sampleCredential()); err != nil {
		t.Fatalf("SaveCredential(personal): %v", err)
	}

	if err := DeleteCredential("work"); err != nil {
		t.Fatalf("DeleteCredential(work): %v", err)
	}

	if _, err := LoadCredential("work"); err != ErrNotLoggedIn {
		t.Errorf("work profile should be gone, got err=%v", err)
	}
	if _, err := LoadCredential("personal"); err != nil {
		t.Errorf("personal profile should survive deleting work: %v", err)
	}
}

// TestDeleteCredential_MissingProfileIsNoError keeps logout idempotent: clearing a
// profile that was never logged in must not error.
func TestDeleteCredential_MissingProfileIsNoError(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := DeleteCredential("never-existed"); err != nil {
		t.Errorf("deleting a missing profile should be a no-op, got %v", err)
	}
}

// A credentials.json written before organizations were kept side by side holds one
// credential per profile. It must load as-is — a release must never log everyone out —
// and be rewritten in the current shape by the next save.
func TestLoadCredential_MigratesTheLegacyShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	legacy := `{"default": {"access_token": "ter_cli_old", "refresh_token": "ter_clr_old",
		"expires_at": "2030-01-01T00:00:00Z", "organization_id": "org-a", "user_email": "d@example.com"}}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, err := LoadCredential("default")
	if err != nil {
		t.Fatalf("LoadCredential: %v", err)
	}
	if cred.AccessToken != "ter_cli_old" || cred.OrganizationID != "org-a" {
		t.Fatalf("legacy credential not read back: %+v", cred)
	}
	if _, err := LoadCredentialFor("default", "org-a"); err != nil {
		t.Fatalf("legacy credential should be addressable by organization: %v", err)
	}

	// Any write rewrites the file in the current shape, and the legacy entry survives
	// beside the new one.
	if _, err := SaveCredential("default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]struct {
		Active        string                     `json:"active"`
		Organizations map[string]json.RawMessage `json:"organizations"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("rewritten file is not in the current shape: %v\n%s", err, data)
	}
	if file["default"].Active != "org-b" || len(file["default"].Organizations) != 2 {
		t.Fatalf("expected org-b active beside org-a, got %+v", file["default"])
	}
}

func TestSaveCredential_KeepsOneCredentialPerOrganization(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if _, err := SaveCredential("default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	replaced, err := SaveCredential("default", orgCredential("org-b", "s-b"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced != nil {
		t.Fatalf("signing into a second organization replaced nothing, got %+v", replaced)
	}

	// The newest login is active; the earlier one is kept for switching back.
	active, err := LoadCredential("default")
	if err != nil || active.OrganizationID != "org-b" {
		t.Fatalf("active = %+v, %v; want org-b", active, err)
	}
	parked, err := LoadCredentialFor("default", "org-a")
	if err != nil || parked.SessionID != "s-a" {
		t.Fatalf("org-a credential should be kept: %+v, %v", parked, err)
	}
	all, err := Credentials("default")
	if err != nil || len(all) != 2 || all[0].OrganizationID != "org-b" {
		t.Fatalf("Credentials should list both, active first: %+v, %v", all, err)
	}

	// A fresh login into an organization that already has a session reports the one
	// it displaced, so the caller can revoke it.
	replaced, err = SaveCredential("default", orgCredential("org-a", "s-a2"))
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.SessionID != "s-a" {
		t.Fatalf("want the displaced org-a session reported, got %+v", replaced)
	}
}

func TestUseOrganization_SwitchesTheActiveCredential(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if _, err := SaveCredential("default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential("default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}

	cred, err := UseOrganization("default", "org-a")
	if err != nil || cred.SessionID != "s-a" {
		t.Fatalf("UseOrganization = %+v, %v", cred, err)
	}
	if active, _ := LoadCredential("default"); active.OrganizationID != "org-a" {
		t.Fatalf("active should now be org-a, got %+v", active)
	}
	if _, err := UseOrganization("default", "org-never"); err != ErrNotLoggedIn {
		t.Fatalf("an organization never signed into is ErrNotLoggedIn, got %v", err)
	}
}

// A refresh persists the rotated token pair for whichever credential was in use —
// which, mid-switch, may not be the active one. Flipping the active organization as
// a side effect of a refresh would change what every other command does next.
func TestUpdateCredential_DoesNotChangeTheActiveOrganization(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if _, err := SaveCredential("default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential("default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}

	rotated := orgCredential("org-a", "s-a")
	rotated.AccessToken = "ter_cli_rotated"
	if err := UpdateCredential("default", rotated); err != nil {
		t.Fatal(err)
	}
	if active, _ := LoadCredential("default"); active.OrganizationID != "org-b" {
		t.Fatalf("refresh switched the active organization: %+v", active)
	}
	if parked, _ := LoadCredentialFor("default", "org-a"); parked.AccessToken != "ter_cli_rotated" {
		t.Fatalf("rotated token not persisted: %+v", parked)
	}
}

func TestDeleteCredentialFor_FallsBackToAnotherOrganization(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if _, err := SaveCredential("default", orgCredential("org-a", "s-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCredential("default", orgCredential("org-b", "s-b")); err != nil {
		t.Fatal(err)
	}

	if err := DeleteCredentialFor("default", "org-b"); err != nil {
		t.Fatal(err)
	}
	active, err := LoadCredential("default")
	if err != nil || active.OrganizationID != "org-a" {
		t.Fatalf("deleting the active credential should fall back to org-a, got %+v, %v", active, err)
	}
	if err := DeleteCredentialFor("default", "org-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential("default"); err != ErrNotLoggedIn {
		t.Fatalf("nothing left should read as not logged in, got %v", err)
	}
}
