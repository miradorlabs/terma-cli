package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// keyPolicy is the policy the auth host serves for the key's team.
const keyPolicy = `{"policy":{"version":"1.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":true},` +
	`"per_repository":{"repositories":["github.com/acme/app"]}}},"revision":3,"updated_at":"2026-09-30T12:27:05Z"}`

// rotatedKey is a second key for the same team, as a rotation mints.
const rotatedKey = "ter_srv_fedcba9876543210"

// serveServerKey answers as the auth host does key, an Acme team's server key: whoami names
// its team and permissions, and it reads that team's policy and nothing else.
func (f *fakeAuth) serveServerKey(w http.ResponseWriter, r *http.Request, key string) {
	team := f.serverKeys[key]
	switch {
	case r.URL.Path == "/v1/whoami":
		f.whoamis.Add(1)
		id := map[string]any{"organization_id": orgA().ID, "project_id": team, "auth_type": "server_key"}
		perms, listed := f.keyPermissions[key]
		if !listed {
			perms = map[string]bool{"read": false, "write": false, "ingest": true}
		}
		if perms != nil {
			id["permissions"] = perms
		}
		_ = json.NewEncoder(w).Encode(id)
	case r.URL.Path == "/v1/policy" && r.URL.Query().Has("project_id"):
		// A key is bound to its team, so naming one is refused, as the auth host does.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"INVALID_ARGUMENT","message":"a server key reads its own team's policy: omit project_id"}}`)
	case r.URL.Path == "/v1/policy":
		f.keyPolicies.Add(1)
		fmt.Fprint(w, cmp.Or(f.policyBody, `{"policy":null}`))
	default:
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"PERMISSION_DENIED","message":"not for this key"}}`)
	}
}

// keySandbox is a machine nobody signed in on, with TERMA_API_KEY set to a key for Acme
// API, the team it returns; the auth host also knows rotatedKey for that team.
func keySandbox(t *testing.T) (*fakeAuth, string) {
	t.Helper()
	gateway := newFakeAuth(t)
	team := projectsIn(orgA().ID)[1].ID
	gateway.serverKeys = map[string]string{testServerKey: team, rotatedKey: team}
	gateway.policyBody = keyPolicy
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	t.Setenv("TERMA_ENV", config.EnvDev)
	t.Setenv("TERMA_API_KEY", testServerKey)
	return gateway, team
}

func setupWithKey(t *testing.T) {
	t.Helper()
	if out, err := runTerma(t, "setup", "--yes", "--harness", "claude"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
}

// refreshAsTheRelay refreshes team's policy as the running relay does, once a minute.
func refreshAsTheRelay(t *testing.T, team string) {
	t.Helper()
	if err := testApp.relayDeps().Refresher().Refresh(t.Context(), team); err != nil {
		t.Fatalf("the relay's policy refresh: %v", err)
	}
}

// TERMA_API_KEY sets the machine up as a browser sign-in does, with the key: the profile
// records the key's organization and team and the environment, the team's policy is
// stored, and the key is kept as the team's key, with nothing minted and no
// credentials.json. Then nothing reads TERMA_API_KEY: with it unset, the relay refreshes
// the policy with the key, doctor, status and config show the machine signed in, and
// teardown undoes setup.
func TestSetupWithAServerKey(t *testing.T) {
	gateway, team := keySandbox(t)
	out, err := runTerma(t, "setup", "--yes", "--harness", "claude")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Signed in     server key "+keystore.Mask(testServerKey)) || !strings.Contains(out, "Team          "+team) || strings.Contains(out, "can also") {
		t.Errorf("setup did not say it signed in with the Ingest-only key, for its team, uncautioned:\n%s", out)
	}
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p == nil || !p.ServerKeySignIn || p.Team != team || p.OrganizationID != orgA().ID || p.Environment != config.EnvDev || p.ServerKeyAuthURL != gateway.srv.URL {
		t.Fatalf("profile = %+v", p)
	}
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != testServerKey {
		t.Fatalf("team key = %q, %v", key, err)
	}
	if _, err := os.Stat(config.CredentialsPath(testApp.dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("setup with a server key wrote credentials.json: %v", err)
	}
	if n := gateway.keysMint.Load(); n != 0 {
		t.Fatalf("setup with a server key minted %d keys", n)
	}

	t.Setenv("TERMA_API_KEY", "")
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if pol := cfg.Policy; !pol.Validated() || pol.TeamID != team || pol.OrganizationID != orgA().ID || pol.Revision != 3 || !pol.IncludePrompts || pol.IncludeToolContent {
		t.Fatalf("stored policy = %+v", pol)
	}
	fetched := gateway.keyPolicies.Load()
	refreshAsTheRelay(t, team)
	if gateway.keyPolicies.Load() != fetched+1 {
		t.Fatal("the relay did not refresh the policy with the team's key")
	}
	masked := regexp.QuoteMeta(keystore.Mask(testServerKey))
	doctorOut, _ := runTerma(t, "doctor")
	if !regexp.MustCompile(`signed in +server key ` + masked + ` in ` + orgA().ID + ` \[dev\]\n`).MatchString(doctorOut) {
		t.Errorf("doctor did not count the key as signed in:\n%s", doctorOut)
	}
	statusOut, _ := runTerma(t, "status")
	if !regexp.MustCompile(`Account: +server key `+masked+` in `+orgA().ID).MatchString(statusOut) || strings.Contains(statusOut, "not signed in") {
		t.Errorf("status did not count the key as signed in:\n%s", statusOut)
	}
	if showOut, err := runTerma(t, "config", "show"); err != nil || !regexp.MustCompile(`"auth": "server key `+masked+`"`).MatchString(showOut) {
		t.Errorf("config show: %v\n%s", err, showOut)
	}
	if out, err := runTerma(t, "teardown", "--yes"); err != nil {
		t.Fatalf("teardown: %v\n%s", err, out)
	}
}

// teardown --sign-out signs a machine set up with a server key out: doctor and status stop
// counting the key as its sign-in, the output says the key itself is revoked in the web
// app, and a browser login left behind the key is revoked and deleted too.
func TestSignOutOfAServerKey(t *testing.T) {
	for _, behindLogin := range []bool{false, true} {
		t.Run(fmt.Sprintf("behind a login %v", behindLogin), func(t *testing.T) {
			gateway, _ := keySandbox(t)
			if behindLogin {
				if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
					t.Fatal(err)
				}
			}
			setupWithKey(t)
			t.Setenv("TERMA_API_KEY", "")
			out, err := runTerma(t, "teardown", "--sign-out", "--yes")
			if err != nil || !strings.Contains(out, "revoke it in the Terma web app") || strings.Contains(out, "Already signed out") {
				t.Fatalf("teardown --sign-out: %v\n%s", err, out)
			}
			if statusOut, _ := runTerma(t, "status"); !strings.Contains(statusOut, "not signed in") {
				t.Errorf("status after signing out of the key:\n%s", statusOut)
			}
			if doctorOut, _ := runTerma(t, "doctor"); !regexp.MustCompile(`(?m)signed in.*no credential`).MatchString(doctorOut) {
				t.Errorf("doctor after signing out of the key:\n%s", doctorOut)
			}
			if revokes := gateway.revokes.Load(); behindLogin && revokes != 1 || !behindLogin && revokes != 0 {
				t.Errorf("revokes = %d; want the login behind the key revoked, and nothing else", revokes)
			}
			if creds, err := auth.Credentials(testApp.dir, config.DefaultProfile); err != nil || len(creds) != 0 {
				t.Errorf("credentials left after sign-out: %d, %v", len(creds), err)
			}
		})
	}
}

// Setup refuses, saying why, before anything is written: a key the auth host does not
// know, a credential that is no team server key, a key for no team or that cannot ingest,
// and an --org or --team that is not the key's own id.
func TestSetupWithAServerKeyRefuses(t *testing.T) {
	const (
		unknown  = "ter_srv_ffffffffffffffff"
		mirador  = "mir_srv_0123456789abcdef"
		teamless = "ter_srv_aaaaaaaaaaaaaaaa"
		noIngest = "ter_srv_bbbbbbbbbbbbbbbb"
		bare     = "ter_srv_cccccccccccccccc"
	)
	team := projectsIn(orgA().ID)[1].ID
	other := projectsIn(orgA().ID)[0].ID
	for _, tc := range []struct {
		name, key string
		args      []string
		want      string
	}{
		{"an unknown or revoked key", unknown, nil, "check the server key in TERMA_API_KEY"},
		{"a Mirador key", mirador, nil, "not to a team server key"},
		{"a CLI token", "ter_cli_" + orgA().ID, nil, "not to a team server key"},
		{"a key for no team", teamless, nil, "belongs to no team"},
		{"a key that cannot ingest", noIngest, nil, "with the Ingest permission"},
		{"a key with no permissions", bare, nil, "with the Ingest permission"},
		{"another organization's id", testServerKey, []string{"--org", orgB().ID}, "unset it to sign in to " + orgB().ID + " as a person"},
		{"an organization by name", testServerKey, []string{"--org", "acme"}, "--org acme: under a server key, setup cannot look up an organization by name — pass its id, or leave --org out"},
		{"another team's id", testServerKey, []string{"--team", other}, "--team " + other + ": the server key in TERMA_API_KEY is team " + team + "'s"},
		{"a team by name", testServerKey, []string{"--team", "Acme API"}, "--team Acme API: under a server key, setup cannot look up a team by name — pass its id, or leave --team out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway, _ := keySandbox(t)
			maps.Copy(gateway.serverKeys, map[string]string{mirador: team, teamless: "", noIngest: team, bare: team})
			gateway.keyPermissions = map[string]map[string]bool{noIngest: {"read": true, "write": true, "ingest": false}, bare: nil}
			t.Setenv("TERMA_API_KEY", tc.key)
			out, err := runTerma(t, append([]string{"setup", "--yes", "--harness", "claude"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("setup = %v, want %q\n%s", err, tc.want, out)
			}
			if file, err := config.LoadFile(testApp.dir); err != nil || file.Profiles[config.DefaultProfile] != nil {
				t.Fatalf("a refused setup recorded the profile: %v", err)
			}
			if key, _ := keystore.Get(testApp.dir, team); key != "" {
				t.Fatal("a refused setup stored the key")
			}
			if !strings.HasPrefix(tc.key, "ter_srv_") && gateway.whoamis.Load() != 0 {
				t.Fatal("a credential that is no team server key was sent to the auth host")
			}
		})
	}
}

// A key that can also read or write the team's data is taken, here by its own --org and
// --team ids, with a caution that an Ingest-only key is enough.
func TestSetupWithAReadServerKeyCautions(t *testing.T) {
	gateway, team := keySandbox(t)
	gateway.keyPermissions = map[string]map[string]bool{testServerKey: {"read": true, "write": false, "ingest": true}}
	out, err := runTerma(t, "setup", "--yes", "--harness", "claude", "--org", orgA().ID, "--team", team)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "it can also read the team's data, and anything on this machine can use it: an Ingest-only key is enough") {
		t.Errorf("setup did not caution about the key:\n%s", out)
	}
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != testServerKey {
		t.Fatalf("team key = %q, %v", key, err)
	}
}

// The key replaces the agents' own keys for its team as well, which the relay prefers: a
// session Claude Code reports then goes out with the server key.
func TestSetupWithAServerKeyReplacesTheAgentsKeys(t *testing.T) {
	_, team := keySandbox(t)
	if err := keystore.SetFor(testApp.dir, "claude", team, rotatedKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	setupWithKey(t)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := testApp.relayDeps().Resolver(cfg, nil)(claim.Claim{ProjectID: team, Tool: "claude-code"})
	if err != nil || pol.Key != testServerKey {
		t.Fatalf("the relay sends Claude Code's session with %q, %v; want the server key", pol.Key, err)
	}
}

// A key whoami takes but whose policy fetch fails leaves the profile, a browser sign-in's
// for another team, and the keystore exactly as they were, --insecure-storage included.
func TestAFailedServerKeySetupChangesNothing(t *testing.T) {
	gateway, team := keySandbox(t)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.SelectOrganization(orgA().ID, orgA().Name)
		p.PinEnvironment(config.EnvDev)
		p.Team, p.Harnesses = projectsIn(orgA().ID)[0].ID, []string{"codex"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(testApp.dir, "claude", team, rotatedKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	before := machineFiles(t)
	gateway.policyBody = "unavailable"
	if out, err := runTerma(t, "setup", "--yes", "--harness", "claude", "--insecure-storage"); err == nil || gateway.keyPolicies.Load() != 1 {
		t.Fatalf("setup with the policy fetch failing: %v\n%s", err, out)
	}
	if after := machineFiles(t); after != before {
		t.Fatalf("a failed setup changed the machine's sign-in:\n%s\nwas\n%s", after, before)
	}
	// The agents are recorded before the policy is fetched, as every setup records them.
	if file, err := config.LoadFile(testApp.dir); err != nil || !slices.Equal(file.Profiles[config.DefaultProfile].Harnesses, []string{"claude"}) {
		t.Fatalf("recorded agents = %v, %v", file.Profiles[config.DefaultProfile].Harnesses, err)
	}
}

// A setup that fails choosing the agents leaves where secrets are kept as it was.
func TestAServerKeySetupFailingOnItsAgentsKeepsTheSecretStorage(t *testing.T) {
	keySandbox(t)
	if out, err := runTerma(t, "setup", "--yes", "--harness", "not-an-agent", "--insecure-storage"); err == nil {
		t.Fatalf("setup with an unknown agent: %v\n%s", err, out)
	}
	if config.InsecureStorage(testApp.dir) {
		t.Fatal("a failed setup recorded --insecure-storage")
	}
}

// machineFiles is the machine's sign-in as setup leaves it: keys.json, credentials.json and
// config.json, whose agent choice is left out.
func machineFiles(t *testing.T) string {
	t.Helper()
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range file.Profiles {
		p.Harnesses = nil
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	all := string(data)
	for _, p := range []string{filepath.Join(testApp.dir, "keys.json"), config.CredentialsPath(testApp.dir)} {
		data, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		all += string(data)
	}
	return all
}

// Setup again without --insecure-storage moves what an earlier --insecure-storage setup kept
// in plain text into the keychain: the key in keys.json, the rotated key with it, and a
// browser login kept behind the key in credentials.json.
func TestServerKeySetupMovesTheKeyWhereSecretsAreKept(t *testing.T) {
	gateway, team := keySandbox(t)
	if out, err := runTerma(t, "setup", "--yes", "--harness", "claude", "--insecure-storage"); err != nil {
		t.Fatalf("setup --insecure-storage: %v\n%s", err, out)
	}
	login := storedSession(gateway, orgA())
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, login); err != nil {
		t.Fatal(err)
	}
	keysFile, credentialsFile := filepath.Join(testApp.dir, "keys.json"), config.CredentialsPath(testApp.dir)
	inPlainText := func(file, secret string) bool {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(string(data), secret)
	}
	if !inPlainText(keysFile, testServerKey) || !inPlainText(credentialsFile, login.AccessToken) || !inPlainText(credentialsFile, login.RefreshToken) {
		t.Fatal("--insecure-storage kept the key or the login elsewhere")
	}
	t.Setenv("TERMA_API_KEY", rotatedKey)
	setupWithKey(t)
	if inPlainText(keysFile, testServerKey) || inPlainText(keysFile, rotatedKey) || inPlainText(credentialsFile, login.AccessToken) || inPlainText(credentialsFile, login.RefreshToken) {
		t.Fatal("a secret stayed in plain text after setup chose the keychain")
	}
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != rotatedKey {
		t.Fatalf("team key = %q, %v; want the rotated one", key, err)
	}
	if cred, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != nil || cred.AccessToken != login.AccessToken || cred.RefreshToken != login.RefreshToken {
		t.Fatalf("the login behind the key: %v", err)
	}
}

// On a machine with no keychain to use, setup without --insecure-storage keeps the key in
// keys.json all the same, says so, and records it, as a browser sign-in does for a login.
func TestServerKeySetupWithNoKeychainSaysTheKeyIsInPlainText(t *testing.T) {
	_, team := keySandbox(t)
	secret.FailForTest(t, testApp.dir)
	out, err := runTerma(t, "setup", "--yes", "--harness", "claude")
	if err != nil || !strings.Contains(out, "no system keychain could be used") {
		t.Fatalf("setup with no keychain: %v\n%s", err, out)
	}
	if !keystore.StoredInFile(testApp.dir, team) || !config.InsecureStorage(testApp.dir) {
		t.Fatal("the key is not recorded as kept in keys.json")
	}
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != testServerKey {
		t.Fatalf("team key = %q, %v", key, err)
	}
}

// A login kept behind the key that the keychain will not take, while it takes the key, stays
// in credentials.json: setup records that secrets are kept in files, as a browser sign-in
// does, rather than leave the login in plain text unrecorded.
func TestServerKeySetupRecordsALoginTheKeychainRefuses(t *testing.T) {
	gateway, team := keySandbox(t)
	if out, err := runTerma(t, "setup", "--yes", "--harness", "claude", "--insecure-storage"); err != nil {
		t.Fatalf("setup --insecure-storage: %v\n%s", err, out)
	}
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	secret.RefuseForTest(t, testApp.dir, "credential/")
	t.Setenv("TERMA_API_KEY", rotatedKey)
	setupWithKey(t)
	if keystore.StoredInFile(testApp.dir, team) {
		t.Fatal("the keychain took the key, so it should have moved there")
	}
	if !auth.StoredInFile(testApp.dir, config.DefaultProfile) || !config.InsecureStorage(testApp.dir) {
		t.Fatal("a login left in plain text is not recorded")
	}
}

// A rotation while the keychain holding the team's current key will not give it up is
// refused before anything is written: the keystore could not compare the keys, and would
// keep the old key's hosts for the new one. Once the keychain opens, the rotation goes
// through.
func TestServerKeyRotationWaitsForALockedKeychain(t *testing.T) {
	_, team := keySandbox(t)
	setupWithKey(t)
	before := machineFiles(t)
	unlock := secret.FailForTest(t, testApp.dir)
	t.Setenv("TERMA_API_KEY", rotatedKey)
	if out, err := runTerma(t, "setup", "--yes", "--harness", "claude"); err == nil || !strings.Contains(out+err.Error(), "unlock the system keychain") {
		t.Fatalf("rotation with the keychain locked: %v\n%s", err, out)
	}
	if after := machineFiles(t); after != before {
		t.Fatalf("a refused rotation changed the machine:\n%s\nwas\n%s", after, before)
	}
	unlock()
	setupWithKey(t)
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != rotatedKey {
		t.Fatalf("team key = %q, %v; want the rotated one", key, err)
	}
}

// Setup again with a rotated key replaces the old one.
func TestSetupWithARotatedServerKeyReplacesTheOld(t *testing.T) {
	_, team := keySandbox(t)
	setupWithKey(t)
	t.Setenv("TERMA_API_KEY", rotatedKey)
	setupWithKey(t)
	if key, err := keystore.Get(testApp.dir, team); err != nil || key != rotatedKey {
		t.Fatalf("team key = %q, %v; want the rotated one", key, err)
	}
}

// A server-key setup over a signed-in profile takes over from the login, which stays on
// disk unused; signing in as a person again takes it back.
func TestServerKeySetupAndSignInTakeTurns(t *testing.T) {
	gateway, team := keySandbox(t)
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(gateway, orgA())); err != nil {
		t.Fatal(err)
	}
	setupWithKey(t)
	if cred, err := auth.LoadCredential(testApp.dir, config.DefaultProfile); err != nil || cred.OrganizationID != orgA().ID {
		t.Fatalf("the login was not kept: %+v, %v", cred, err)
	}
	t.Setenv("TERMA_API_KEY", "")
	logins, keyed := gateway.policies.Load(), gateway.keyPolicies.Load()
	refreshAsTheRelay(t, team)
	if gateway.policies.Load() != logins || gateway.keyPolicies.Load() != keyed+1 {
		t.Fatal("a profile set up with a server key refreshed its policy with the login")
	}

	setupWithKey(t)
	if cfg, err := testApp.loadConfig(); err != nil || cfg.ServerKeySignIn || cfg.Team != team {
		t.Fatalf("after signing in as a person: %+v, %v", cfg, err)
	}
	logins, keyed = gateway.policies.Load(), gateway.keyPolicies.Load()
	refreshAsTheRelay(t, team)
	if gateway.policies.Load() != logins+1 || gateway.keyPolicies.Load() != keyed {
		t.Fatal("a signed-in profile refreshed its policy with the server key")
	}
}

// A profile pointed at another auth host after a server-key setup is not signed in there:
// doctor and status say so and name the key-mode setup, as the policy refresh refuses.
func TestServerKeyProfileOnAnotherAuthHost(t *testing.T) {
	keySandbox(t)
	setupWithKey(t)
	t.Setenv("TERMA_API_KEY", "")
	t.Setenv("TERMA_AUTH_URL", "https://auth.elsewhere.example")
	doctorOut, _ := runTerma(t, "doctor")
	if !strings.Contains(doctorOut, "set up against a different auth host") || !strings.Contains(doctorOut, "TERMA_API_KEY=") {
		t.Errorf("doctor on another auth host:\n%s", doctorOut)
	}
	if statusOut, _ := runTerma(t, "status"); !strings.Contains(statusOut, "server key set up against a different auth host") {
		t.Errorf("status on another auth host:\n%s", statusOut)
	}
}
