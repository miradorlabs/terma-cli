package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
)

func TestParseOrgRef(t *testing.T) {
	if ref := parseOrgRef(orgA().ID); ref.ID != orgA().ID || ref.Name != "" {
		t.Fatalf("a UUID is an id: %+v", ref)
	}
	if ref := parseOrgRef("  Acme "); ref.Name != "Acme" || ref.ID != "" {
		t.Fatalf("anything else is a name: %+v", ref)
	}
	if !parseOrgRef("").empty() {
		t.Fatal("blank is empty")
	}
}

func TestMatchOrganization(t *testing.T) {
	if o, err := matchOrganization(fakeOrgs, orgB().ID); err != nil || o.Name != "Beta Labs" {
		t.Fatalf("by id: %+v, %v", o, err)
	}
	if o, err := matchOrganization(fakeOrgs, "beta"); err != nil || o.ID != orgB().ID {
		t.Fatalf("by unique prefix: %+v, %v", o, err)
	}
	if _, err := matchOrganization(fakeOrgs, "zeta"); err == nil || !strings.Contains(err.Error(), "no organization matches") {
		t.Fatalf("unknown: %v", err)
	}
	ambiguous := append([]organization{{ID: "3", Name: "Acme Two"}}, fakeOrgs...)
	if o, err := matchOrganization(ambiguous, "acme"); err != nil || o.Name != "Acme" {
		t.Fatalf("an exact name wins over a longer one that shares the prefix: %+v, %v", o, err)
	}
	if _, err := matchOrganization(ambiguous, "ac"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("an ambiguous prefix must not guess: %v", err)
	}
}

// /v1/organizations' type tells the person's own workspace from a shared organization,
// whatever the names: the picker says "personal" beside it, and the role beside a shared
// one.
func TestOrganizationTypeNamesThePersonalWorkspace(t *testing.T) {
	var list listOrganizationsResponse
	body := `{"organizations":[{"id":"s","name":"Personal","role":"admin","type":"shared"},` +
		`{"id":"p","name":"Personal","role":"admin","type":"personal"},{"id":"o","name":"Old","role":"member"}]}`
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, o := range list.Organizations {
		notes = append(notes, o.note())
	}
	if got := strings.Join(notes, ","); got != "admin,personal,member" {
		t.Fatalf("notes = %s, want admin,personal,member", got)
	}
}

// signInHere runs the sign-in `terma setup` starts with, under a deadline that cuts a
// browser handoff short; it returns what it printed.
func signInHere(t *testing.T, opts signInOptions, wait time.Duration) (*auth.Credential, string, error) {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	cred, err := testApp.signIn(cmd, cfg, opts)
	return cred, out.String(), err
}

// chooseHere runs setup's organization choice for the stored sign-in, asking with ask.
func chooseHere(t *testing.T, want orgRef, ask orgAsker) (int, error) {
	t.Helper()
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := auth.LoadCredential(testApp.dir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	return testApp.chooseOrganization(context.Background(), cfg, cred, want, ask)
}

// A working stored session is verified and reused; no browser opens.
func TestSignInReusesAWorkingSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}

	cred, out, err := signInHere(t, signInOptions{noBrowser: true}, 10*time.Second)
	if err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if cred.UserID != "u-1" || strings.Contains(out, "Open this URL") {
		t.Fatalf("sign-in should reuse the stored session without a browser:\n%s", out)
	}
	if f.whoamis.Load() != 1 {
		t.Fatalf("the session should be verified exactly once, got %d whoami calls", f.whoamis.Load())
	}
}

// The one sign-in reads every organization: --org picks one by name or id, with no browser
// and no second sign-in, and selects no team.
func TestChooseOrganizationWithOneSignIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		ref  string
		want organization
	}{{"beta labs", orgB()}, {"beta", orgB()}, {orgA().ID, orgA()}} {
		if _, err := chooseHere(t, parseOrgRef(step.ref), nil); err != nil {
			t.Fatalf("--org %s: %v", step.ref, err)
		}
		file, _ := config.LoadFile(testApp.dir)
		if p := file.Profiles[config.DefaultProfile]; p.OrganizationID != step.want.ID || p.Team != "" {
			t.Fatalf("--org %s: profile %+v", step.ref, p)
		}
	}
	if f.exchanges.Load() != 0 || f.revokes.Load() != 0 {
		t.Fatalf("choosing an organization signed in again: %d exchanges, %d revokes", f.exchanges.Load(), f.revokes.Load())
	}

	// With no --org and no question to ask, the one chosen before is kept among the person's.
	kept, err := chooseHere(t, orgRef{}, nil)
	if err != nil || kept != 2 {
		t.Fatalf("kept = %d, %v; want the current one kept among 2", kept, err)
	}
	// With none chosen, several organizations need --org or a question.
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.SelectOrganization("", "") }); err != nil {
		t.Fatal(err)
	}
	if _, err := chooseHere(t, orgRef{}, nil); err == nil || !strings.Contains(err.Error(), "pass --org") {
		t.Fatalf("no organization chosen among two: %v", err)
	}
	asked := false
	if _, err := chooseHere(t, orgRef{}, func(orgs []organization, _ string) (*organization, error) {
		asked = true
		return &orgs[1], nil
	}); err != nil || !asked {
		t.Fatalf("a terminal is asked: %v, asked %v", err, asked)
	}
	if file, _ := config.LoadFile(testApp.dir); file.Profiles[config.DefaultProfile].OrganizationID != orgB().ID {
		t.Fatal("the organization picked is recorded")
	}
}

// A retryable answer listing the organizations is an error that changes nothing: it never
// reads as the person having none.
func TestChooseOrganizationKeepsTheChoiceWhenUnavailable(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(orgA().ID, "Acme")
		p.Team = projectsIn(orgA().ID)[0].ID
	}); err != nil {
		t.Fatal(err)
	}
	f.orgsDown = true
	if _, err := chooseHere(t, orgRef{}, nil); err == nil {
		t.Fatal("an unavailable organization list must be an error")
	}
	if p := mustProfile(t); p.OrganizationID != orgA().ID || p.Team == "" {
		t.Fatalf("a retryable answer dropped the choice: %+v", p)
	}
}

func mustProfile(t *testing.T) *config.Profile {
	t.Helper()
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	return file.Profiles[config.DefaultProfile]
}

// A session the server no longer honours is forgotten and the sign-in falls through to
// the browser, whose link asks for a user session and names no organization.
func TestSignInDropsADeadSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	f.deadToken = fakeUserToken("u-1")

	_, out, err := signInHere(t, signInOptions{org: parseOrgRef("Beta Labs"), noBrowser: true}, 2*time.Second)
	if err == nil {
		t.Fatalf("a dead session should fall through to the browser, which the deadline cuts short:\n%s", out)
	}
	if !strings.Contains(out, "Open this URL") || !strings.Contains(out, "protocol=user") || strings.Contains(out, "org=") {
		t.Fatalf("the browser handoff should have started for a user session:\n%s", out)
	}
	if _, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != auth.ErrNotLoggedIn {
		t.Fatalf("the dead session should be forgotten, got %v", err)
	}
}

// A new sign-in is stored before the session it replaces is ended: one session per
// machine, and a crash in between leaves this machine signed in.
func TestStoreLoginEndsTheReplacedSessionAfterStoring(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	old := storedSession(f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, old); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	fresh := storedSession(f)
	fresh.RefreshToken, fresh.SessionID = refreshTokenPrefix+"fresh", "s-fresh"
	if err := testApp.storeLogin(context.Background(), cfg, fresh); err != nil {
		t.Fatal(err)
	}
	if got := f.revokedTokens(); len(got) != 1 || got[0] != old.RefreshToken {
		t.Fatalf("revoked %d tokens, want the replaced session's refresh token alone", len(got))
	}
	if stored, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != nil || stored.SessionID != "s-fresh" {
		t.Fatalf("stored %+v, %v; want the new sign-in", stored, err)
	}
}

// Signing in retires what an older terma signed in with: each organization's session is
// revoked with its own token and forgotten, and the team keys it minted go, but a server
// key a profile was set up with stays.
func TestStoreLoginRetiresAnOlderTermasSignIns(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	legacyA, legacyB := legacyTokenPrefix+orgA().ID, legacyTokenPrefix+orgB().ID
	legacy := map[string]any{"default": map[string]any{"active": orgA().ID, "organizations": map[string]any{
		orgA().ID: map[string]any{"access_token": legacyA, "session_id": "old-a", "auth_url": f.srv.URL, "organization_id": orgA().ID},
		orgB().ID: map[string]any{"access_token": legacyB, "session_id": "old-b", "auth_url": f.srv.URL, "organization_id": orgB().ID},
	}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	minted, kept := projectsIn(orgA().ID)[0].ID, projectsIn(orgB().ID)[0].ID
	for _, team := range []string{minted, kept} {
		if err := keystore.Set(testApp.dir, team, testServerKey, keystore.Hosts{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.UpdateProfile(testApp.dir, "headless", func(p *config.Profile) { p.ServerKeySignIn, p.Team = true, kept }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	got := f.legacyRevokedTokens()
	slices.Sort(got)
	want := []string{legacyA, legacyB}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("revoked %d older sessions, want both, each with its own token", len(got))
	}
	if auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older sign-ins should be deleted")
	}
	if key, _ := keystore.Get(testApp.dir, minted); key != "" {
		t.Fatal("a team key an older terma minted should be deleted")
	}
	if key, _ := keystore.Get(testApp.dir, kept); key == "" {
		t.Fatal("a server key a profile signed in with should stay")
	}
}

// Retiring an older terma's sign-ins waits for their record: it is the only account what
// that terma queued keeps once they are gone, so when it can't be written they stay, unrevoked,
// for the next sign-in to retire.
func TestAnUnrecordedRetirementKeepsTheOlderSignIns(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	legacyA := legacyTokenPrefix + orgA().ID
	legacy := map[string]any{"default": map[string]any{"active": orgA().ID, "organizations": map[string]any{
		orgA().ID: map[string]any{"access_token": legacyA, "session_id": "old-a", "auth_url": f.srv.URL, "organization_id": orgA().ID, "user_id": "u-1"},
	}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the record goes: writing it fails.
	record := filepath.Join(testApp.stateDir, auth.LegacyRecordFile)
	if err := os.MkdirAll(filepath.Join(record, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(record) })
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if !auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older sign-ins were deleted with no record of whose they were")
	}
	if got := f.legacyRevokedTokens(); len(got) != 0 {
		t.Fatalf("revoked %d older sessions before recording them", len(got))
	}
}

// A session's refresh token is revoked only at the auth host this profile is configured
// for: a credential naming another host, as a tampered file might, sends it nowhere.
func TestRevokeSendsTheTokenOnlyToTheConfiguredHost(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write([]byte(`{"enforcement":"ENFORCED"}`))
	}))
	t.Cleanup(other.Close)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cred := storedSession(f)
	cred.AuthURL = other.URL
	if err := testApp.revokeSession(context.Background(), cfg, cred); err == nil {
		t.Fatal("a session from another host was revoked")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the refresh token went to another host %d times", n)
	}
	if err := testApp.revokeSession(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatalf("revoke at the configured host: %v", err)
	}
	if got := f.revokedTokens(); len(got) != 1 {
		t.Fatalf("revoked %d at the configured host, want 1", len(got))
	}
}

// After an update, a person's command says why nothing is signed in: the sign-ins the
// older terma kept are retired. Setup, which signs in, and a signed-in machine say nothing.
func TestAnUpdatedMachineIsToldToSignIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	notice := func(name string) string {
		var out bytes.Buffer
		cmd := &cobra.Command{Use: name}
		cmd.SetErr(&out)
		testApp.tellSignInAfterUpdate(cmd, true)
		return out.String()
	}
	if got := notice("status"); !strings.Contains(got, "terma was updated") {
		t.Fatalf("notice = %q, want it to say the update needs a sign-in", got)
	}
	if got := notice("setup"); got != "" {
		t.Fatalf("setup repeated the notice: %q", got)
	}
	// A command that fails for want of a sign-in, which skips the notice, says it instead.
	if got := testApp.notSignedIn(); !strings.Contains(got, "terma was updated") {
		t.Fatalf("not signed in = %q, want it to say the update needs a sign-in", got)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if got := notice("status"); got != "" {
		t.Fatalf("a signed-in machine was told to sign in: %q", got)
	}
}

// An older terma's file that names no sign-in that can be read is retired with a record
// that names no one: what that terma queued is no server key's to send later.
func TestAnUnreadableOlderFileIsRetiredAsNoOnes(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older file outlived the sign-in")
	}
	if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || rec.Account != auth.LegacyAccountAmbiguous || rec.RetiredAt.IsZero() {
		t.Fatalf("record = %+v, %v; want one naming no one", rec, err)
	}
}

// An older file that doesn't read whole is retired as no one's, but what could be read of it
// is still revoked: its sessions don't stay live with nothing pointing at them.
func TestAPartlyBrokenOlderFileIsRetiredAsNoOnes(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	legacyA := legacyTokenPrefix + orgA().ID
	data := `{"default": {"organizations": {"` + orgA().ID + `": {"access_token": "` + legacyA + `", "session_id": "old-a", "auth_url": "` + f.srv.URL + `", "organization_id": "` + orgA().ID + `", "user_id": "u-1"}}}, "work": {"organizations": "not a map"}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if got := f.legacyRevokedTokens(); len(got) != 1 || got[0] != legacyA {
		t.Fatalf("revoked %d older sessions, want the readable one", len(got))
	}
	if rec, err := auth.ReadLegacyRecord(testApp.stateDir); err != nil || rec.Account != auth.LegacyAccountAmbiguous ||
		len(rec.SessionIDs) != 1 || rec.SessionIDs[0] != "old-a" {
		t.Fatalf("record = %+v, %v; want one naming no one, keeping the readable session for the heartbeat", rec, err)
	}
	if auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older file outlived the sign-in")
	}
}

// The older sign-ins' boundary is when their retirement is done, after their revokes: what
// an older terma's hook still running queued meanwhile stays on that terma's side.
func TestTheLegacyBoundaryIsSetOnceTheRetirementIsDone(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	legacyA := legacyTokenPrefix + orgA().ID
	data := `{"default": {"organizations": {"` + orgA().ID + `": {"access_token": "` + legacyA + `", "session_id": "old-a", "auth_url": "` + f.srv.URL + `", "organization_id": "` + orgA().ID + `", "user_id": "u-1"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Int64
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/cli/revoke" {
			time.Sleep(50 * time.Millisecond)
			revoked.Store(time.Now().UnixNano())
		}
		inner.ServeHTTP(w, r)
	})
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	rec, err := auth.ReadLegacyRecord(testApp.stateDir)
	if err != nil || revoked.Load() == 0 || !rec.RetiredAt.After(time.Unix(0, revoked.Load())) {
		t.Fatalf("boundary %v, revoked at %v (%v); want the boundary after the revoke", rec.RetiredAt, time.Unix(0, revoked.Load()), err)
	}
}

// With the older sign-ins still on the machine, because the keychain would not let one go,
// an older hook may still be queueing: every unbound event in the spool stays theirs, for
// delivery and the hooks alike. The relay, whose server-key parts are unbound, keeps the
// record's boundary, which is the end of the attempt.
func TestEverythingUnboundStaysLegacyWhileTheOlderSignInsRemain(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	legacyA := legacyTokenPrefix + orgA().ID
	data := `{"default": {"organizations": {` +
		`"` + orgA().ID + `": {"access_token": "` + legacyA + `", "session_id": "old-a", "auth_url": "` + f.srv.URL + `", "organization_id": "` + orgA().ID + `", "user_id": "u-1"},` +
		`"` + orgB().ID + `": {"session_id": "old-b", "auth_url": "` + f.srv.URL + `", "organization_id": "` + orgB().ID + `", "user_id": "u-1"}}}}`
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Int64
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/cli/revoke" {
			time.Sleep(50 * time.Millisecond)
			revoked.Store(time.Now().UnixNano())
		}
		inner.ServeHTTP(w, r)
	})
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// The second older sign-in's tokens are in the keychain, which won't let them go.
	secret.FailForTest(t, testApp.dir)
	if err := testApp.storeLogin(context.Background(), cfg, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	if !auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older sign-ins were forgotten although the keychain kept one")
	}
	rec, err := auth.ReadLegacyRecord(testApp.stateDir)
	if err != nil || revoked.Load() == 0 || !rec.RetiredAt.After(time.Unix(0, revoked.Load())) {
		t.Fatalf("boundary %v, revoked at %v (%v); want it after the attempt", rec.RetiredAt, time.Unix(0, revoked.Load()), err)
	}
	for _, serverKey := range []bool{false, true} {
		if got := spoolLegacyOwnership(testApp.dir, testApp.stateDir, serverKey); !got.RetiredAt.Equal(auth.LegacyStillSignedIn) {
			t.Errorf("server key %v: delivery judges by %v; want everything unbound held", serverKey, got.RetiredAt)
		}
	}
	if got, err := auth.LegacyOwnership(testApp.dir, testApp.stateDir, true); err != nil || !got.RetiredAt.Equal(rec.RetiredAt) {
		t.Errorf("the relay judges by %v (%v), want the record's %v", got.RetiredAt, err, rec.RetiredAt)
	}
	if got := legacyBefore(testApp.dir, testApp.stateDir); !got.Equal(farFuture) {
		t.Errorf("the hooks judge by %v; want everything unbound held", got)
	}
	// Once they are gone the record's own boundary counts again.
	if err := os.Remove(config.LegacyCredentialsPath(testApp.dir)); err != nil {
		t.Fatal(err)
	}
	if got := spoolLegacyOwnership(testApp.dir, testApp.stateDir, true); !got.RetiredAt.Equal(rec.RetiredAt) {
		t.Errorf("after the sign-ins went: %v, want %v", got.RetiredAt, rec.RetiredAt)
	}
	if got := legacyBefore(testApp.dir, testApp.stateDir); !got.Equal(rec.RetiredAt) {
		t.Errorf("hooks after the sign-ins went: %v, want %v", got, rec.RetiredAt)
	}
}

// `terma teardown --sign-out` revokes the session server-side and forgets it.
func TestSignOutRevokesTheSession(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := testApp.signOut(cmd); err != nil {
		t.Fatalf("sign out: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "Signed out.") {
		t.Fatalf("sign-out should say so:\n%s", &out)
	}
	if got := f.revokedTokens(); len(got) != 1 || got[0] != storedSession(f).RefreshToken {
		t.Fatalf("the session must be revoked server-side with its refresh token, got %d revokes", len(got))
	}
	if _, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != auth.ErrNotLoggedIn {
		t.Fatalf("the sign-in should be gone: %v", err)
	}
}

// A sign-in whose last refresh got no answer may hold a spent refresh token: signing out
// sends it nowhere, since a spent token at revoke is reuse and ends the whole family, and
// still forgets it.
func TestSignOutNeverPresentsATokenInDoubt(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	cred := storedSession(f)
	cred.RefreshInDoubtSince = time.Now()
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, cred); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := testApp.signOut(cmd); err != nil {
		t.Fatalf("sign out: %v\n%s", err, &out)
	}
	if f.revokes.Load() != 0 || !strings.Contains(out.String(), "left to expire") {
		t.Fatalf("%d revokes; output:\n%s", f.revokes.Load(), &out)
	}
	if _, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != auth.ErrNotLoggedIn {
		t.Fatalf("the sign-in should be gone: %v", err)
	}
}

// A retirement cut short is tried again when the stored sign-in is reused; an older
// session filed for another auth host is forgotten unrevoked, its token sent nowhere.
func TestReusingASignInRetiresWhatIsLeftOfAnOlderTerma(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := config.UpdateFile(testApp.dir, func(file *config.File) { file.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	here, elsewhere := legacyTokenPrefix+"here", legacyTokenPrefix+"elsewhere"
	legacy := map[string]any{"default": map[string]any{"organizations": map[string]any{
		orgA().ID: map[string]any{"access_token": here, "session_id": "old-a", "auth_url": f.srv.URL, "organization_id": orgA().ID},
		orgB().ID: map[string]any{"access_token": elsewhere, "session_id": "old-b", "auth_url": "https://auth.elsewhere.example", "organization_id": orgB().ID},
	}}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(config.LegacyCredentialsPath(testApp.dir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, out, err := signInHere(t, signInOptions{noBrowser: true}, 10*time.Second); err != nil {
		t.Fatalf("sign-in: %v\n%s", err, out)
	}
	if got := f.legacyRevokedTokens(); !slices.Equal(got, []string{here}) {
		t.Fatalf("revoked %d older sessions, want only the configured host's", len(got))
	}
	if auth.HasLegacySignIns(testApp.dir) {
		t.Fatal("the older sign-ins should be deleted")
	}
}

// A retired sign-in is not reused: `terma login` forgets it and opens the browser, rather
// than fail on it until its token expires.
func TestReuseForgetsARetiredSignIn(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	retired := storedSession(f)
	retired.RetiredAt = time.Now().UTC()
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, retired); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cred, ok, err := testApp.reuseStoredSession(t.Context(), cfg, nil); ok || err != nil || cred != nil {
		t.Fatalf("reuse = %v, %v; want the browser opened", ok, err)
	}
	if _, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("the retired sign-in is still stored (%v)", err)
	}
}

// A sign-in another setup stored while this one verified the stored session ends the reuse:
// the session it verified is over, and setting up with it would mix two people's.
func TestReuseRefusesASessionReplacedMeanwhile(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	inner := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			other := storedSessionOf(f, "u-2")
			if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, other); err != nil {
				t.Error(err)
			}
		}
		inner.ServeHTTP(w, r)
	})
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := testApp.reuseStoredSession(t.Context(), cfg, nil); ok || err == nil || !strings.Contains(err.Error(), "signed in again") {
		t.Fatalf("reuse = %v, %v; want it refused", ok, err)
	}
}
