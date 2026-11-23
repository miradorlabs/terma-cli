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

// Whose an older terma's backlog is: a server-key machine's is no person's, whatever its
// sign-ins say; else the one person they all name, from a file that read whole; else no
// one's.
func TestLegacyOwner(t *testing.T) {
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
		signIns          []LegacySignIn
		whole, serverKey bool
		want             string
	}{
		"one person":            {of("user-1", "user-1"), true, false, AccountKey(host, "user-1")},
		"two people":            {of("user-1", "user-2"), true, false, LegacyAccountAmbiguous},
		"an unknown one":        {of(""), true, false, LegacyAccountAmbiguous},
		"none":                  {nil, true, false, LegacyAccountAmbiguous},
		"a file not read whole": {of("user-1"), false, false, LegacyAccountAmbiguous},
		"a server key":          {of("user-1"), true, true, LegacyNoPerson},
		"a server key, unread":  {nil, false, true, LegacyNoPerson},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := LegacyOwner(tc.signIns, tc.whole, tc.serverKey); got != tc.want {
				t.Fatalf("owner = %q; want %q", got, tc.want)
			}
		})
	}
}

// The first decision written stands: a later retirement, of another person or as a server
// key, only adds the sessions it read, and a record it adds none to is not written again.
func TestRecordLegacy_TheFirstDecisionStands(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	first := time.Now().Add(-time.Hour).UTC()
	if err := RecordLegacy(stateDir, []LegacySignIn{{SessionID: "s-a"}}, "0123456789abcdef", first); err != nil {
		t.Fatal(err)
	}
	if err := RecordLegacy(stateDir, []LegacySignIn{{SessionID: "s-a"}, {SessionID: "s-b"}}, LegacyNoPerson, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadLegacyRecord(stateDir)
	if err != nil || rec.Account != "0123456789abcdef" || !rec.RetiredAt.Equal(first) || !slices.Equal(rec.SessionIDs, []string{"s-a", "s-b"}) {
		t.Fatalf("record = %+v, %v; want the first decision with s-b added", rec, err)
	}
	path := filepath.Join(stateDir, LegacyRecordFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordLegacy(stateDir, []LegacySignIn{{SessionID: "s-b"}}, LegacyAccountAmbiguous, time.Now()); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("a record kept as it was was written again: %v", err)
	}
}

// A record that can't be read is not taken for none: a later retirement keeps it, rather
// than decide again.
func TestRecordLegacy_KeepsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, LegacyRecordFile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	signIns := []LegacySignIn{{SessionID: "s", UserID: "u", AuthURL: "https://auth.example.test"}}
	if err := RecordLegacy(dir, signIns, AccountKey("https://auth.example.test", "u"), time.Now()); err == nil {
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

// A server-key profile's look records the key's decision, no person's, with or without
// older sign-ins, and their sessions for the heartbeat; a later retirement as their person
// keeps it. A person's profile records nothing: its backlog waits for the retirement at
// sign-in.
func TestLegacyOwnership_AServerKeyDecidesNoPerson(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
	data := `{"default": {"organizations": {"o": {"session_id": "s", "auth_url": "https://auth.example.test", "organization_id": "o", "user_id": "u"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec, err := LegacyOwnership(dir, stateDir, false); err != nil || rec.Account != "" {
		t.Fatalf("a person's profile: %+v, %v; want nothing recorded", rec, err)
	}
	first, err := LegacyOwnership(dir, stateDir, true)
	if err != nil || first.Account != LegacyNoPerson || !slices.Equal(first.SessionIDs, []string{"s"}) {
		t.Fatalf("a server key's look: %+v, %v; want no person's, with the session", first, err)
	}
	signIns, err := LegacySignIns(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordLegacy(stateDir, signIns, LegacyOwner(signIns, true, false), time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadLegacyRecord(stateDir); err != nil || rec.Account != LegacyNoPerson {
		t.Fatalf("after a sign-in: %+v, %v; want the server key's decision kept", rec, err)
	}
	if rec, err := LegacyOwnership(t.TempDir(), t.TempDir(), true); err != nil || rec.Account != LegacyNoPerson {
		t.Fatalf("no older sign-ins: %+v, %v; want no person's all the same", rec, err)
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
	if _, whole, err := ReadLegacySignIns(dir, false); err != nil || whole {
		t.Fatalf("a partly broken file read whole (%v)", err)
	}
	// What can be read is still listed, to be revoked and forgotten.
	if signIns, err := LegacySignIns(dir, false); err != nil || len(signIns) != 1 {
		t.Fatalf("LegacySignIns = %+v, %v; want the readable one", signIns, err)
	}
}

// A server key's look never overwrites a record a retirement writes at the same moment:
// it writes only while there is still no decision, under the record's lock.
func TestLegacyOwnership_NeverOverwritesARecordWrittenMeanwhile(t *testing.T) {
	t.Parallel()
	dir, stateDir := t.TempDir(), t.TempDir()
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
	want := AccountKey("https://auth.example.test", "u")
	if err := config.WriteJSON(filepath.Join(stateDir, LegacyRecordFile), LegacyRecord{Account: want, RetiredAt: time.Now()}, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()
	got := <-done
	if rec, err := ReadLegacyRecord(stateDir); err != nil || rec.Account != want || got.Account != want {
		t.Fatalf("record = %+v (%v), look = %+v; want the retirement's account kept", rec, err, got)
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

// A file that can't be read is an error, not a file that names no one: nothing is decided
// from it. One there is read once, with whether it parsed whole.
func TestReadLegacySignIns_AReadErrorDecidesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(config.LegacyCredentialsPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLegacySignIns(dir, false); err == nil {
		t.Fatal("an unreadable file read")
	}
}

// The lane of a decision: a person's key and no person's are their own, no decision is
// none, and anything else is ambiguous.
func TestLegacyLane(t *testing.T) {
	t.Parallel()
	for decision, want := range map[string]string{
		"":                     "",
		"0123456789abcdef":     "0123456789abcdef",
		LegacyNoPerson:         LegacyNoPerson,
		LegacyAccountAmbiguous: LegacyAccountAmbiguous,
		"someone":              LegacyAccountAmbiguous,
		AccountUnreadable:      LegacyAccountAmbiguous,
	} {
		if got := LegacyLane(decision); got != want {
			t.Errorf("lane of %q = %q, want %q", decision, got, want)
		}
	}
}

// The decision is taken only once every older queue is set aside: with a stage that fails,
// as one does while an older terma's flush holds its lock, nothing is recorded, and the next
// try sets them aside and decides. A decision taken stands, and sets nothing aside again.
func TestRecordLegacyDecidesOnlyOnceEveryQueueIsSetAside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	staged := map[string]int{}
	stages := func(outbox error) []LegacyStage {
		return []LegacyStage{
			{Queue: LegacySpool, Stage: func() error { staged[LegacySpool]++; return nil }},
			{Queue: LegacyOutbox, Stage: func() error { staged[LegacyOutbox]++; return outbox }},
		}
	}
	if err := RecordLegacy(dir, nil, LegacyNoPerson, time.Now(), stages(errors.New("held"))...); err == nil {
		t.Fatal("a decision whose outbox could not be set aside was recorded")
	}
	if rec, err := ReadLegacyRecord(dir); err != nil || rec.Account != "" {
		t.Fatalf("record = %+v, %v; want none", rec, err)
	}
	for _, owner := range []string{LegacyNoPerson, LegacyAccountAmbiguous} {
		if err := RecordLegacy(dir, nil, owner, time.Now(), stages(nil)...); err != nil {
			t.Fatal(err)
		}
	}
	if rec, _ := ReadLegacyRecord(dir); rec.Account != LegacyNoPerson || staged[LegacySpool] != 2 || staged[LegacyOutbox] != 2 {
		t.Fatalf("record = %+v, staged %v; want the first decision taken, each queue set aside twice", rec, staged)
	}
}
