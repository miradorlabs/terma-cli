package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// The desktop's user, a UUID as Terma Desktop sends it. Beta Labs, with its one team, plays
// the PERSONAL organization onboarding passes; Acme, with two, a SHARED organization.
const deskUser = "33333333-3333-4333-8333-333333333333"

func personalOrg() organization { return orgB() }
func personalTeam() string      { return projectsIn(orgB().ID)[0].ID }
func teamOrgTeam() string       { return projectsIn(orgA().ID)[0].ID }

// grantSandbox is a machine with no terma sign-in, against a fake auth host.
func grantSandbox(t *testing.T) *fakeAuth {
	t.Helper()
	gateway := newFakeAuth(t)
	authSandbox(t, gateway)
	sandboxMachine(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	return gateway
}

// identityLine is the desktop's first line on stdin.
func identityLine(user, org string) string {
	return `{"user_id":"` + user + `","org_id":"` + org + `"}` + "\n"
}

// desktopSetup runs `terma setup -y --desktop-grant` for org and team as Terma Desktop
// does, with stdin holding what the desktop writes.
func desktopSetup(t *testing.T, stdin, org, team string) (stdout, stderr string, err error) {
	t.Helper()
	return termaRun{within: 30 * time.Second, stdin: strings.NewReader(stdin)}.exec(t,
		"setup", "-y", "--desktop-grant", "--org", org, "--team", team, "--harness", "codex")
}

// firstLine is stdout's first line as JSON.
func firstLine(t *testing.T, stdout string) map[string]any {
	t.Helper()
	line, _, _ := strings.Cut(stdout, "\n")
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("stdout's first line is not JSON: %q\n%s", line, stdout)
	}
	return v
}

// storeSignIn stores a sign-in of user's, as an earlier setup would have.
func storeSignIn(t *testing.T, f *fakeAuth, user string) {
	t.Helper()
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSessionOf(f, user)); err != nil {
		t.Fatal(err)
	}
}

// grantFailure is the code of a failed grant's {"error": …} line, and whether it exited 1.
func grantFailure(t *testing.T, stdout string, err error) string {
	t.Helper()
	if code, ok := exitCodeOf(err); !ok || code != 1 {
		t.Fatalf("a failed grant exits 1 having said why on stdout; err = %v\n%s", err, stdout)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var failure struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if json.Unmarshal([]byte(lines[len(lines)-1]), &failure) != nil || failure.Error.Message == "" {
		t.Fatalf("stdout's last line is not {\"error\": {code, message}}:\n%s", stdout)
	}
	return failure.Error.Code
}

func TestDesktopGrantTakesOnlyIdsAndNeverAsks(t *testing.T) {
	grantSandbox(t)
	org, team := personalOrg().ID, personalTeam()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--org", org, "--team", team}, "needs -y"},
		{[]string{"-y", "--team", team}, "needs --org"},
		{[]string{"-y", "--org", "Beta Labs", "--team", team}, "needs --org"},
		{[]string{"-y", "--org", org}, "needs --team"},
		{[]string{"-y", "--org", org, "--team", "Beta Core"}, "needs --team"},
		{[]string{"-y", "--org", org, "--team", team, "--no-browser"}, "drop --no-browser"},
	} {
		args := append([]string{"setup", "--desktop-grant", "--harness", "codex"}, tc.args...)
		stdout, _, err := termaRun{stdin: strings.NewReader(identityLine(deskUser, org))}.exec(t, args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) || stdout != "" {
			t.Errorf("%v: err = %v, stdout %q; want %q and nothing on stdout", tc.args, err, stdout, tc.want)
		}
	}
	t.Setenv("TERMA_API_KEY", testServerKey)
	if _, _, err := desktopSetup(t, identityLine(deskUser, org), org, team); err == nil || !strings.Contains(err.Error(), "TERMA_API_KEY") {
		t.Fatalf("a server key in the environment must refuse the grant: %v", err)
	}
}

// desktopGrantJSON is the auth package's copy of terma-desktop's delegatedGrant, read before
// any test moves the working directory.
var desktopGrantJSON, desktopGrantErr = os.ReadFile("../account/auth/testdata/delegated_grant.json")

// desktopLines are the lines terma-desktop's auth.json delegatedGrant pins, by case or run
// name: what the CLI prints, as the desktop's tests feed it to the app.
func desktopLines(t *testing.T) map[string]string {
	t.Helper()
	raw, err := desktopGrantJSON, desktopGrantErr
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		DelegatedGrant struct {
			Cases []struct{ Name, Out string }
			Runs  []struct {
				Name     string
				CLIError string `json:"cliError"`
			}
		} `json:"delegatedGrant"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	lines := map[string]string{}
	for _, c := range f.DelegatedGrant.Cases {
		lines[c.Name] = c.Out
	}
	for _, r := range f.DelegatedGrant.Runs {
		if r.CLIError != "" {
			lines[r.Name] = r.CLIError
		}
	}
	return lines
}

// The desktop's identity, when it names an organization, must name --org; the failure is
// the line the desktop's fixture pins.
func TestDesktopGrantIdentityForAnotherOrganizationIsBadInput(t *testing.T) {
	gateway := grantSandbox(t)
	stdout, _, err := desktopSetup(t, identityLine(deskUser, orgA().ID), personalOrg().ID, personalTeam())
	if code := grantFailure(t, stdout, err); code != auth.GrantBadInput {
		t.Fatalf("code = %s, want %s", code, auth.GrantBadInput)
	}
	if want := desktopLines(t)["the CLI's own error"]; stdout != want+"\n" || gateway.exchanges.Load() != 0 {
		t.Fatalf("stdout = %q, want only the desktop's pinned line %q", stdout, want)
	}
}

// Onboarding: the desktop's code signs the CLI in to the PERSONAL organization, and the rest
// of setup runs as ever. Nothing secret is printed, and the session says where it came from.
func TestDesktopGrantSignsInAndSetsUp(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.grant("the-delegated-code", deskUser)
	stdout, stderr, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"the-delegated-code"}`+"\n",
		personalOrg().ID, personalTeam())
	if err != nil {
		t.Fatalf("setup: %v\n%s%s", err, stdout, stderr)
	}
	req := firstLine(t, stdout)
	challenge, _ := req["challenge"].(string)
	if len(req) != 2 || len(challenge) != 43 || req["label"] != auth.DesktopLabel() {
		t.Fatalf("first line = %v, want {challenge, label}", req)
	}
	verifiers := gateway.verifiersSent()
	if gateway.exchanges.Load() != 1 || len(verifiers) != 1 || s256(verifiers[0]) != challenge {
		t.Fatalf("the code must be redeemed once with the verifier of the challenge printed (%d exchanges)", gateway.exchanges.Load())
	}
	if installations := gateway.installationsSent(); len(installations) != 1 || installations[0] != config.StoredInstallationID(testApp.dir) {
		t.Fatalf("the exchange must name this installation: %v", installations)
	}
	cred, err := auth.LoadCredential(testApp.dir, config.DefaultProfile)
	if err != nil || cred.UserID != deskUser || cred.Label != auth.DesktopLabel() {
		t.Fatalf("stored sign-in = %+v, %v", cred, err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil || cfg.OrganizationID != personalOrg().ID || cfg.Team != personalTeam() || !cfg.Policy.Validated() {
		t.Fatalf("profile = org %s team %s policy %+v, %v", cfg.OrganizationID, cfg.Team, cfg.Policy, err)
	}
	// Doctor tells this sign-in from a browser one.
	if probed, err := testApp.doctorProbes(cfg).Credential(); err != nil || !probed.ViaDesktop {
		t.Fatalf("doctor's credential = %+v, %v; want it signed in through Terma Desktop", probed, err)
	}
	for _, want := range []string{"Signed in     dev@example.com (Beta Labs)", "Agents", "Collects"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("setup's own output does not say %q:\n%s", want, stdout)
		}
	}
	// Task 4's redaction check: the code, the verifier and the tokens never leave.
	for name, secret := range map[string]string{"code": "the-delegated-code", "verifier": verifiers[0],
		"access token": fakeUserToken(deskUser), "refresh token": refreshTokenPrefix + "granted_1"} {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Errorf("the %s reached the output:\n%s\n%s", name, stdout, stderr)
		}
	}
}

// A stored sign-in of the same user is reused: no grant, no exchange.
func TestDesktopGrantReusesTheSameUsersSignIn(t *testing.T) {
	gateway := grantSandbox(t)
	storeSignIn(t, gateway, deskUser)
	stdout, stderr, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID), personalOrg().ID, personalTeam())
	if err != nil {
		t.Fatalf("setup: %v\n%s%s", err, stdout, stderr)
	}
	if line := firstLine(t, stdout); len(line) != 1 || line["reused"] != true {
		t.Fatalf("first line = %v, want {\"reused\":true}", line)
	}
	if gateway.exchanges.Load() != 0 || gateway.revokes.Load() != 0 {
		t.Fatalf("a reuse exchanges and ends nothing: %d exchanges, %d revokes", gateway.exchanges.Load(), gateway.revokes.Load())
	}
	// A sign-in made in the browser stays one, reused or not.
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if probed, err := testApp.doctorProbes(cfg).Credential(); err != nil || probed.ViaDesktop {
		t.Fatalf("doctor's credential = %+v, %v; want a browser sign-in", probed, err)
	}
}

// A reused sign-in is announced only once --org is checked: refused, stdout carries the
// {"error": …} line and no {"reused":true}, as a fresh code's refusal does.
func TestDesktopGrantChecksTheOrganizationBeforeAnnouncingReuse(t *testing.T) {
	for name, orgs := range map[string][]organization{"someone else's": {orgA()}, "none": {}} {
		t.Run(name, func(t *testing.T) {
			gateway := grantSandbox(t)
			gateway.orgs = orgs
			storeSignIn(t, gateway, deskUser)
			stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID), personalOrg().ID, personalTeam())
			if code := grantFailure(t, stdout, err); code != auth.GrantBadInput {
				t.Fatalf("code = %s, want %s", code, auth.GrantBadInput)
			}
			if strings.Contains(stdout, "reused") {
				t.Fatalf("a refused reuse was announced:\n%s", stdout)
			}
		})
	}
}

// A reuse the grant refuses for --org changes nothing: a profile set up with a server key,
// with a person's sign-in behind it, stays set up with the key.
func TestARefusedReuseLeavesTheProfileAlone(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.orgs = []organization{orgA()}
	storeSignIn(t, gateway, deskUser)
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.ServerKeySignIn, p.ServerKeyAuthURL = true, gateway.srv.URL
	}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID), personalOrg().ID, personalTeam())
	if code := grantFailure(t, stdout, err); code != auth.GrantBadInput {
		t.Fatalf("code = %s, want %s", code, auth.GrantBadInput)
	}
	file, err := config.LoadFile(testApp.dir)
	if err != nil {
		t.Fatal(err)
	}
	if p := file.Profiles[config.DefaultProfile]; p == nil || !p.ServerKeySignIn || p.ServerKeyAuthURL != gateway.srv.URL {
		t.Fatalf("profile = %+v; a refused grant changed it", p)
	}
}

// An id spelled in uppercase is the same user: Terma's ids are lowercase, so the desktop's
// identity is read without regard to case and the stored sign-in is still reused.
func TestDesktopGrantReadsIdsWithoutRegardToCase(t *testing.T) {
	gateway := grantSandbox(t)
	const user = "abcdef01-2345-4678-89ab-cdef01234567"
	storeSignIn(t, gateway, user)
	stdout, stderr, err := desktopSetup(t, identityLine(strings.ToUpper(user), personalOrg().ID), personalOrg().ID, personalTeam())
	if err != nil {
		t.Fatalf("setup: %v\n%s%s", err, stdout, stderr)
	}
	if line := firstLine(t, stdout); line["reused"] != true || gateway.exchanges.Load() != 0 || gateway.revokes.Load() != 0 {
		t.Fatalf("first line = %v with %d exchanges and %d revokes, want the stored sign-in reused", line,
			gateway.exchanges.Load(), gateway.revokes.Load())
	}
}

// Another user's sign-in is never reused: the desktop is asked for a grant, and the
// stored sign-in is replaced and ended.
func TestDesktopGrantAsksWhenTheStoredSignInIsSomeoneElses(t *testing.T) {
	gateway := grantSandbox(t)
	storeSignIn(t, gateway, "u-1")
	gateway.grant("c1", deskUser)
	stdout, stderr, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if err != nil {
		t.Fatalf("setup: %v\n%s%s", err, stdout, stderr)
	}
	if _, ok := firstLine(t, stdout)["challenge"]; !ok || gateway.exchanges.Load() != 1 {
		t.Fatalf("want a grant, got:\n%s", stdout)
	}
	if !slices.Contains(gateway.revokedTokens(), storedSessionOf(gateway, "u-1").RefreshToken) {
		t.Fatal("the other user's stored sign-in must be ended")
	}
	if active, _ := auth.LoadCredential(testApp.dir, config.DefaultProfile); active == nil || active.UserID != deskUser {
		t.Fatalf("active sign-in = %+v, want the desktop user's", active)
	}
}

// A desktop that names no organization on stdin, as one on user sessions does, is taken
// at --org.
func TestDesktopGrantTakesAnIdentityWithoutAnOrganization(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.grant("c1", deskUser)
	stdout, stderr, err := desktopSetup(t, `{"user_id":"`+deskUser+`"}`+"\n"+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if err != nil {
		t.Fatalf("setup: %v\n%s%s", err, stdout, stderr)
	}
	if org, team, n := selected(t); org != personalOrg().ID || team != personalTeam() || n != 1 {
		t.Fatalf("org %s team %s, %d sign-ins", org, team, n)
	}
}

// --org must be an organization the signed-in user belongs to.
func TestDesktopGrantRefusesAnOrganizationOfSomeoneElse(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.orgs = []organization{orgA()}
	gateway.grant("c1", deskUser)
	stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if code := grantFailure(t, stdout, err); code != auth.GrantBadInput {
		t.Fatalf("code = %s, want %s", code, auth.GrantBadInput)
	}
	if org, _, n := selected(t); org != "" || n != 0 {
		t.Fatalf("an organization of someone else's was chosen (%s), or the refused sign-in kept (%d)", org, n)
	}
	if gateway.revokes.Load() != 1 {
		t.Fatalf("%d revokes; want the refused grant's session ended", gateway.revokes.Load())
	}
}

// A code that signs in someone else than the desktop's user is never kept: it is ended,
// and the run fails with grant_identity_mismatch.
func TestDesktopGrantKeepsNoSignInForAnotherIdentity(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.grant("c1", "44444444-4444-4444-8444-444444444444")
	stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if code := grantFailure(t, stdout, err); code != auth.GrantIdentityMismatch {
		t.Fatalf("code = %s, want %s", code, auth.GrantIdentityMismatch)
	}
	if _, _, n := selected(t); n != 0 || gateway.revokes.Load() != 1 {
		t.Fatalf("%d sign-ins stored and %d ended, want none stored and the minted one ended", n, gateway.revokes.Load())
	}
}

// The other failures the desktop reads by code.
func TestDesktopGrantFailures(t *testing.T) {
	for _, tc := range []struct {
		name, code, want string
	}{
		{"EOF before the code", "", auth.GrantNoCode},
		{"a code the auth host refuses", `{"code":"spent"}` + "\n", auth.GrantRejected},
		{"a code line that is not JSON", "spent\n", auth.GrantBadInput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grantSandbox(t)
			stdout, stderr, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+tc.code, personalOrg().ID, personalTeam())
			if code := grantFailure(t, stdout, err); code != tc.want {
				t.Fatalf("code = %s, want %s\n%s", code, tc.want, stdout)
			}
			if _, ok := firstLine(t, stdout)["challenge"]; !ok {
				t.Fatalf("the request comes first:\n%s", stdout)
			}
			if !strings.Contains(stderr, tc.want) || strings.Contains(stdout+stderr, "spent") {
				t.Fatalf("stderr names the code and nothing quotes what was sent:\n%s\n%s", stdout, stderr)
			}
			// The auth host refuses a code with a 401; that is no sign-out, so nothing says one.
			if strings.Contains(stderr, "not logged in") {
				t.Fatalf("a refused code reads as a sign-out:\n%s", stderr)
			}
			if pinned := desktopLines(t)["no code to give: stdin closes, and the CLI exits 1 with grant_no_code"]; tc.want == auth.GrantNoCode &&
				!strings.HasSuffix(stdout, "\n"+pinned+"\n") {
				t.Fatalf("stdout = %q, want it to end with the desktop's pinned line %q", stdout, pinned)
			}
			if _, _, n := selected(t); n != 0 {
				t.Fatalf("%d sign-ins stored after a failed grant", n)
			}
		})
	}
}

// Switching from the desktop: to a SHARED organization by grant, then to Personal and
// back, each reusing the one sign-in, which reads both.
func TestDesktopGrantSwitchesOrganizationsAndBack(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.grant("to-team", deskUser)
	for _, step := range []struct {
		org        organization
		team, code string
		wantReused bool
	}{
		{orgA(), teamOrgTeam(), "to-team", false},
		{personalOrg(), personalTeam(), "", true},
		{orgA(), teamOrgTeam(), "", true},
	} {
		stdin := identityLine(deskUser, step.org.ID)
		if step.code != "" {
			stdin += `{"code":"` + step.code + `"}` + "\n"
		}
		stdout, stderr, err := desktopSetup(t, stdin, step.org.ID, step.team)
		if err != nil {
			t.Fatalf("setup for %s: %v\n%s%s", step.org.Name, err, stdout, stderr)
		}
		if reused := firstLine(t, stdout)["reused"] == true; reused != step.wantReused {
			t.Fatalf("setup for %s: reused = %v, want %v\n%s", step.org.Name, reused, step.wantReused, stdout)
		}
		org, team, n := selected(t)
		if org != step.org.ID || team != step.team || n != 1 || gateway.exchanges.Load() != 1 {
			t.Fatalf("after %s: org %s team %s, %d sign-ins, %d exchanges", step.org.Name, org, team, n, gateway.exchanges.Load())
		}
	}
	if gateway.revokes.Load() != 0 {
		t.Fatalf("switching ended %d sign-ins; the one sign-in serves every organization", gateway.revokes.Load())
	}
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// An organization list that cannot be read is no bad input from the desktop: the run fails
// as setup fails, without grant_bad_input, keeps nothing, and ends nothing, since the
// check may have spent the token.
func TestDesktopGrantDoesNotBlameTheDesktopForAnOutage(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.orgsDown = true
	gateway.grant("c1", deskUser)
	stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if err == nil || strings.Contains(stdout, auth.GrantBadInput) {
		t.Fatalf("err = %v, stdout:\n%s", err, stdout)
	}
	if org, _, n := selected(t); org != "" || n != 0 || gateway.revokes.Load() != 0 {
		t.Fatalf("org %q, %d sign-ins, %d revokes; want nothing chosen, kept or ended", org, n, gateway.revokes.Load())
	}
}

// A user with no organization at all is refused as the desktop's bad input, and the session
// the grant made is ended.
func TestDesktopGrantRefusesAUserWithNoOrganization(t *testing.T) {
	gateway := grantSandbox(t)
	gateway.orgs = []organization{}
	gateway.grant("c1", deskUser)
	stdout, _, err := desktopSetup(t, identityLine(deskUser, personalOrg().ID)+`{"code":"c1"}`+"\n", personalOrg().ID, personalTeam())
	if code := grantFailure(t, stdout, err); code != auth.GrantBadInput {
		t.Fatalf("code = %s, want %s", code, auth.GrantBadInput)
	}
	if _, _, n := selected(t); n != 0 || gateway.revokes.Load() != 1 {
		t.Fatalf("%d sign-ins kept, %d revokes; want none kept and the grant's session ended", n, gateway.revokes.Load())
	}
}
