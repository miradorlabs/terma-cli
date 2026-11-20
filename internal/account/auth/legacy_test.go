package auth

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// writeLegacy leaves what an older terma kept: work's org-a sign-in with its token in
// credentials.json, and default's org-a and org-b sign-ins with theirs in the keychain.
func writeLegacy(t *testing.T, dir string) {
	t.Helper()
	data := `{
  "work": {"active": "org-a", "organizations": {
    "org-a": {"access_token": "access-work-a", "session_id": "s-work-a", "auth_url": "https://auth.example.test", "organization_id": "org-a", "user_id": "user-1"}
  }},
  "default": {"active": "org-b", "organizations": {
    "org-b": {"session_id": "s-default-b", "auth_url": "https://auth.example.test", "organization_id": "org-b", "user_id": "user-1"},
    "org-a": {"session_id": "s-default-a", "auth_url": "https://auth.example.test", "organization_id": "org-a", "user_id": "user-1"}
  }}
}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.LegacyCredentialsPath(dir)+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-a", "org-b"} {
		pair := `{"access_token":"access-default-` + org + `","refresh_token":"refresh-default-` + org + `"}`
		if err := secret.Set(dir, legacyItemName(config.DefaultProfile, org), pair); err != nil {
			t.Fatal(err)
		}
	}
}

// LegacySignIns lists every sign-in credentials.json indexes, ordered by profile then
// organization, with tokens read from the file or, when asked, the keychain.
func TestLegacySignIns_ReadsFileAndKeychainTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeLegacy(t, dir)

	signIns, err := LegacySignIns(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []LegacySignIn{
		{Profile: "default", OrganizationID: "org-a", SessionID: "s-default-a", UserID: "user-1", AuthURL: "https://auth.example.test", AccessToken: "access-default-org-a"},
		{Profile: "default", OrganizationID: "org-b", SessionID: "s-default-b", UserID: "user-1", AuthURL: "https://auth.example.test", AccessToken: "access-default-org-b"},
		{Profile: "work", OrganizationID: "org-a", SessionID: "s-work-a", UserID: "user-1", AuthURL: "https://auth.example.test", AccessToken: "access-work-a", inFile: true},
	}
	if !slices.Equal(signIns, want) {
		t.Fatalf("LegacySignIns(tokens) =\n%+v\nwant\n%+v", signIns, want)
	}

	signIns, err = LegacySignIns(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want[:2] {
		want[i].AccessToken = ""
	}
	if !slices.Equal(signIns, want) {
		t.Fatalf("LegacySignIns(no tokens) =\n%+v\nwant\n%+v", signIns, want)
	}
}

// With no credentials.json there is nothing to list or forget.
func TestLegacySignIns_NoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if HasLegacySignIns(dir) {
		t.Fatal("HasLegacySignIns = true with no credentials.json")
	}
	if signIns, err := LegacySignIns(dir, true); err != nil || signIns != nil {
		t.Fatalf("LegacySignIns = %+v, %v; want none", signIns, err)
	}
	if err := ForgetLegacy(dir); err != nil {
		t.Fatalf("ForgetLegacy = %v; want a no-op", err)
	}
}

// ForgetLegacy deletes every keychain item credentials.json names, then the file and its
// lock, and leaves the current sign-in alone.
func TestForgetLegacy_DeletesItemsFileAndLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := SaveCredential(dir, config.DefaultProfile, sessionCredential("s1")); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, dir)
	if !HasLegacySignIns(dir) {
		t.Fatal("HasLegacySignIns = false with credentials.json present")
	}

	if err := ForgetLegacy(dir); err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if _, err := secret.Get(dir, legacyItemName(config.DefaultProfile, org)); !errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("the older sign-in's item for %s outlived ForgetLegacy: %v", org, err)
		}
	}
	for _, path := range []string{config.LegacyCredentialsPath(dir), config.LegacyCredentialsPath(dir) + ".lock"} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s outlived ForgetLegacy: %v", path, err)
		}
	}
	if HasLegacySignIns(dir) {
		t.Fatal("HasLegacySignIns = true after ForgetLegacy")
	}
	if cred, err := LoadCredential(dir, config.DefaultProfile); err != nil || cred.SessionID != "s1" {
		t.Fatalf("ForgetLegacy touched the current sign-in: %+v, %v", cred, err)
	}
}

// A keychain that will not give up the older items keeps credentials.json, so a later
// sign-in tries again.
func TestForgetLegacy_KeepsTheFileWhenTheKeychainFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeLegacy(t, dir)
	secret.FailForTest(t, dir)

	if err := ForgetLegacy(dir); err == nil {
		t.Fatal("ForgetLegacy reported success with the keychain locked")
	}
	if !HasLegacySignIns(dir) {
		t.Fatal("credentials.json went while its keychain items stayed")
	}
}

// The record keeps the account the retired sign-ins all belonged to. Sign-ins of more than
// one person, or of no known user, name no one: their backlog is held from every sign-in
// and every server key.
func TestRecordLegacy_Account(t *testing.T) {
	t.Parallel()
	const host = "https://auth.example.test"
	of := func(users ...string) []LegacySignIn {
		var signIns []LegacySignIn
		for i, u := range users {
			signIns = append(signIns, LegacySignIn{SessionID: "s-" + string(rune('a'+i)), UserID: u, AuthURL: host})
		}
		return signIns
	}
	for name, tc := range map[string]struct {
		signIns []LegacySignIn
		want    string
	}{
		"one person":     {of("user-1", "user-1"), AccountKey(host, "user-1")},
		"two people":     {of("user-1", "user-2"), LegacyAccountAmbiguous},
		"an unknown one": {of(""), LegacyAccountAmbiguous},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := RecordLegacy(dir, tc.signIns, time.Now()); err != nil {
				t.Fatal(err)
			}
			rec, err := ReadLegacyRecord(dir)
			if err != nil || rec.Account != tc.want {
				t.Fatalf("account = %q, %v; want %q", rec.Account, err, tc.want)
			}
		})
	}
}

// A record that can't be read is not taken for none: a later retirement keeps it, rather
// than give an older terma's backlog the new sign-ins' account.
func TestRecordLegacy_KeepsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, LegacyRecordFile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	signIns := []LegacySignIn{{SessionID: "s", UserID: "u", AuthURL: "https://auth.example.test"}}
	if err := RecordLegacy(dir, signIns, time.Now()); err == nil {
		t.Fatal("an unreadable record was replaced")
	}
	if data, _ := os.ReadFile(path); string(data) != "{" {
		t.Fatalf("record = %q", data)
	}
}

// Sign-ins an older terma kept in the file with no keychain are forgotten without one: the
// keychain has no item of theirs to remove, so its failing keeps nothing.
func TestForgetLegacy_FileHeldSignInsNeedNoKeychain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := `{"default": {"active": "org-a", "organizations": {
    "org-a": {"access_token": "access-a", "session_id": "s-a", "auth_url": "https://auth.example.test", "organization_id": "org-a", "user_id": "user-1"},
    "org-b": {"access_token": "access-b", "session_id": "s-b", "auth_url": "https://auth.example.test", "organization_id": "org-b", "user_id": "user-1"}
  }}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	secret.FailForTest(t, dir)
	if err := ForgetLegacy(dir); err != nil {
		t.Fatalf("ForgetLegacy: %v", err)
	}
	if HasLegacySignIns(dir) {
		t.Fatal("credentials.json outlived the retirement")
	}
}

// While an older terma's sign-ins are on the machine with no record of them (set up with a
// server key first, which records nothing), the record is written the first time this
// terma looks: what was bound to no account until then is theirs, and the boundary never
// moves after. With nothing to record it from, everything bound to no account until now is
// named by no one.
func TestLegacyOwnership_WithoutARecord(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
	if rec, err := LegacyOwnership(dir, stateDir); err != nil || rec.Account != "" || !rec.RetiredAt.IsZero() {
		t.Fatalf("no older sign-ins: %+v, %v; want no record", rec, err)
	}
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nameless, err := LegacyOwnership(dir, stateDir)
	if err != nil || nameless.Account != LegacyAccountAmbiguous || time.Since(nameless.RetiredAt).Abs() > time.Minute {
		t.Fatalf("older sign-ins naming no one: %+v, %v; want everything until now named by no one", nameless, err)
	}
	// Recorded too, so the boundary stays: what a server key collects after is its own.
	if again, err := LegacyOwnership(dir, stateDir); err != nil || !again.RetiredAt.Equal(nameless.RetiredAt) {
		t.Fatalf("second look: %+v, %v; want the boundary recorded at %v", again, err, nameless.RetiredAt)
	}
	stateDir = t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "organization_id": "o", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := LegacyOwnership(dir, stateDir)
	if err != nil || first.Account != AccountKey("https://auth.example.test", "u") || first.RetiredAt.IsZero() {
		t.Fatalf("first look: %+v, %v; want the older sign-ins' account, recorded", first, err)
	}
	// The older terma rewriting its file later moves nothing.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(config.LegacyCredentialsPath(dir), later, later); err != nil {
		t.Fatal(err)
	}
	if again, err := LegacyOwnership(dir, stateDir); err != nil || !again.RetiredAt.Equal(first.RetiredAt) {
		t.Fatalf("second look: %+v, %v; want the boundary recorded at the first, %v", again, err, first.RetiredAt)
	}
}
