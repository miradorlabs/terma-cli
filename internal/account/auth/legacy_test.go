package auth

import (
	"context"
	"errors"
	"github.com/miradorlabs/terma-cli/internal/flock"
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

// ForgetLegacy, under the older terma's lock as a retirement runs it, deletes every keychain
// item credentials.json names, then the file and, once it is let go, its lock; and leaves
// the current sign-in alone.
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

	var forgot error
	if err := WithLegacySignIns(context.Background(), dir, func() { forgot = ForgetLegacy(dir) }); err != nil || forgot != nil {
		t.Fatal(err, forgot)
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

// With older sign-ins on the machine and no record, a server-key profile records one at
// its first look: everything bound to no account until then is no one's, for good, so a
// later sign-in, even as the older sign-ins' person, never sends what the key collected. A
// person's profile records nothing: its backlog waits for the retirement at sign-in.
func TestLegacyOwnership_WithoutARecord(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
	if rec, err := LegacyOwnership(dir, stateDir, true); err != nil || !rec.RetiredAt.IsZero() {
		t.Fatalf("no older sign-ins: %+v, %v; want no record", rec, err)
	}
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "organization_id": "o", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec, err := LegacyOwnership(dir, stateDir, false); err != nil || !rec.RetiredAt.IsZero() {
		t.Fatalf("a person's profile: %+v, %v; want nothing recorded", rec, err)
	}
	first, err := LegacyOwnership(dir, stateDir, true)
	if err != nil || first.Account != LegacyAccountAmbiguous || first.RetiredAt.IsZero() {
		t.Fatalf("a server key's first look: %+v, %v; want everything until now named by no one", first, err)
	}
	// The older person signs in later: the boundary and its no one stay.
	signIns, err := LegacySignIns(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordLegacy(stateDir, signIns, first.RetiredAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadLegacyRecord(stateDir); err != nil || !rec.RetiredAt.Equal(first.RetiredAt) || rec.Account != LegacyAccountAmbiguous {
		t.Fatalf("after a sign-in: %+v, %v; want the server key's boundary kept", rec, err)
	}
}

// A file that doesn't parse whole is not read whole: one readable profile beside a broken
// one is not taken for the whole backlog's person, but it is still listed for cleanup.
func TestLegacySignIns_APartlyBrokenFileNamesNoOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "user_id": "u"}}}, "work": {"organizations": "not a map"}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if LegacyParsedWhole(dir) {
		t.Fatal("a partly broken file read whole")
	}
	// What can be read is still listed, to be revoked and forgotten.
	if signIns, err := LegacySignIns(dir, false); err != nil || len(signIns) != 1 {
		t.Fatalf("LegacySignIns = %+v, %v; want the readable one", signIns, err)
	}
}

// A record already kept stands when the older sign-ins are retired as no one's, and gains
// the sessions that could be read, for the heartbeat.
func TestRecordNoOne_KeepsARecordAndAddsItsSessions(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	first := time.Now().Add(-time.Hour).UTC()
	if err := RecordNoOne(stateDir, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := RecordNoOne(stateDir, []LegacySignIn{{SessionID: "s-a"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil || !rec.RetiredAt.Equal(first) || rec.Account != LegacyAccountAmbiguous || len(rec.SessionIDs) != 1 || rec.SessionIDs[0] != "s-a" {
		t.Fatalf("record = %+v, %v; want the first boundary with s-a added", rec, err)
	}
}

// A server key's first look never overwrites a record a retirement writes at the same
// moment: it writes only while there is still no record, under the record's lock.
func TestLegacyOwnership_NeverOverwritesARecordWrittenMeanwhile(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := flock.Lock(t.Context(), filepath.Join(stateDir, LegacyRecordFile)+".lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan LegacyRecord)
	go func() {
		rec, _ := LegacyOwnership(dir, stateDir, true)
		done <- rec
	}()
	// The retirement records the older sign-ins' own account while the look waits.
	time.Sleep(100 * time.Millisecond)
	signIns, _ := LegacySignIns(dir, false)
	if err := recordLegacy(stateDir, signIns, time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock()
	got := <-done
	want := AccountKey("https://auth.example.test", "u")
	if rec, err := ReadLegacyRecord(stateDir); err != nil || rec.Account != want || got.Account != want {
		t.Fatalf("record = %+v (%v), look = %+v; want the retirement's account kept", rec, err, got)
	}
}

// The boundary moves only from the one its retirement wrote: a record another look or
// retirement wrote keeps its own, and it never moves back.
func TestAdvanceLegacyBoundary_MovesOnlyItsOwn(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	first := time.Now().Add(-time.Minute).UTC()
	if err := RecordNoOne(stateDir, nil, first); err != nil {
		t.Fatal(err)
	}
	later := first.Add(30 * time.Second)
	if err := AdvanceLegacyBoundary(stateDir, first.Add(-time.Hour), later); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ReadLegacyRecord(stateDir); !rec.RetiredAt.Equal(first) {
		t.Fatalf("another's boundary moved: %v", rec.RetiredAt)
	}
	if err := AdvanceLegacyBoundary(stateDir, first, later); err != nil {
		t.Fatal(err)
	}
	if rec, _ := ReadLegacyRecord(stateDir); !rec.RetiredAt.Equal(later) {
		t.Fatalf("boundary = %v, want %v", rec.RetiredAt, later)
	}
}

// A retirement's mark of when it forgot the older sign-ins only moves forward, never touches
// the relay's boundary, and is not made on a record not yet kept.
func TestMarkLegacyForgotten_MovesOnlyForward(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	at := time.Unix(1_800_000_000, 0).UTC()
	if err := MarkLegacyForgotten(stateDir, at); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadLegacyRecord(stateDir); err != nil || !rec.RetiredAt.IsZero() {
		t.Fatalf("a mark with no record wrote %+v, %v", rec, err)
	}
	if err := RecordLegacy(stateDir, []LegacySignIn{{SessionID: "s", AuthURL: "https://auth.example.test", UserID: "u"}}, at); err != nil {
		t.Fatal(err)
	}
	for _, mark := range []time.Time{at.Add(time.Hour), at.Add(time.Minute)} {
		if err := MarkLegacyForgotten(stateDir, mark); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil || !rec.ForgottenAt.Equal(at.Add(time.Hour)) || !rec.RetiredAt.Equal(at) || !rec.ReplayBefore().Equal(at.Add(time.Hour)) {
		t.Fatalf("record = %+v, %v; want forgotten an hour after a boundary left as it was", rec, err)
	}
}

// A retirement waits for an older terma holding credentials.json's lock, as one signing in
// does, and gives up past its wait: nothing is read, recorded or forgotten under a file that
// may be replaced. With the lock free it runs, and once the file is forgotten its lock goes.
func TestARetirementHoldsTheOlderTermasLock(t *testing.T) {
	dir := t.TempDir()
	path := config.LegacyCredentialsPath(dir)
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	was := legacyLockWait
	legacyLockWait = 100 * time.Millisecond
	t.Cleanup(func() { legacyLockWait = was })
	unlock, err := lockCredentialFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := WithLegacySignIns(context.Background(), dir, func() { ran = true }); err == nil || ran {
		t.Fatalf("ran %v, err %v; want it to give up while an older terma holds the lock", ran, err)
	}
	unlock()
	if err := WithLegacySignIns(context.Background(), dir, func() { _ = os.Remove(path) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the lock stayed after the file was forgotten: %v", err)
	}
}
