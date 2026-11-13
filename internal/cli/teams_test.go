package cli

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const (
	acmeWeb  = "aaaaaaaa-0000-4000-8000-000000000001"
	acmeAPI  = "aaaaaaaa-0000-4000-8000-000000000002"
	betaCore = "bbbbbbbb-0000-4000-8000-000000000001"
)

// policyBody is a 1.1 policy response listing repos, as /v1/policy answers it.
func policyBody(repos ...string) string {
	list, _ := json.Marshal(repos)
	return `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"per_repository":{"repositories":` +
		string(list) + `}}},"revision":3,"updated_at":"2026-11-01T00:00:00Z"}`
}

// globalBody is a policy response that collects every session.
const globalBody = `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"global":{}}},"revision":2,"updated_at":"2026-11-01T00:00:00Z"}`

// migratedMachine is a machine already moved to routing by teams, signed in to signedIn,
// with the routing state made by seed, and the fake auth host's policies over HTTP.
func migratedMachine(t *testing.T, seed func(*fakeAuth, *routing.State) error, signedIn ...organization) *fakeAuth {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	f.policyBodies = map[string]string{
		acmeWeb:  policyBody("github.com/acme/web"),
		acmeAPI:  policyBody("github.com/acme/api"),
		betaCore: policyBody("github.com/beta/core"),
	}
	if err := os.WriteFile(config.CredentialsV2Path(testApp.dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, o := range signedIn {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := routing.Create(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
		if seed == nil {
			return nil
		}
		return seed(f, s)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// selectedTeam is a routing team for team in org, its policy listing repos, validated now.
func stateTeam(f *fakeAuth, org, team string, personal bool, repos ...string) routing.Team {
	pol := config.Policy{Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: time.Now().UTC(),
		OrganizationID: org, AuthURL: f.srv.URL, TeamID: team, GitHooks: true}
	if personal && len(repos) == 0 {
		pol.Mode = config.ModeGlobal
	}
	return routing.Team{OrganizationID: org, AuthURL: f.srv.URL, TeamID: team, Personal: personal, Policy: pol}
}

func teamsJSON(t *testing.T, args ...string) (teamsView, string, error) {
	t.Helper()
	stdout, stderr, err := termaRun{}.exec(t, append([]string{"teams", "--json"}, args...)...)
	var v teamsView
	if err == nil {
		if jerr := json.Unmarshal([]byte(stdout), &v); jerr != nil {
			t.Fatalf("teams --json printed no JSON (%v):\n%s\n%s", jerr, stdout, stderr)
		}
	}
	return v, stdout + stderr, err
}

func loadState(t *testing.T) routing.State {
	t.Helper()
	st, ok, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !ok {
		t.Fatalf("routing state: %v, %v", ok, err)
	}
	return st
}

func (v teamsView) row(org, team string) (orgRow, teamRow, bool) {
	for _, o := range v.Organizations {
		for _, t := range o.Teams {
			if o.ID == org && t.ID == team {
				return o, t, true
			}
		}
	}
	return orgRow{}, teamRow{}, false
}

// Listing (contract C4): every organization from one sign-in; a signed-in one's teams with
// their entries; one without a sign-in says so; a team whose policy read says it is gone is
// hidden.
func TestTeamsListsEveryOrganizationAndTheSignedInOnesTeams(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	f.goneTeams = map[string]bool{acmeWeb: true}
	v, out, err := teamsJSON(t)
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	if _, api, ok := v.row(orgA().ID, acmeAPI); !ok || api.Selected || !slices.Equal(api.Entries, []string{"github.com/acme/api"}) || api.Collects != "github.com/acme/api" {
		t.Errorf("Acme API = %+v (listed %v)", api, ok)
	}
	if _, _, ok := v.row(orgA().ID, acmeWeb); ok {
		t.Error("a team whose policy answers TEAM_NOT_FOUND is listed")
	}
	i := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == orgB().ID })
	if i < 0 || v.Organizations[i].SignedIn || len(v.Organizations[i].Teams) != 0 || v.Organizations[i].Note != "sign in to see its teams" {
		t.Errorf("Beta Labs, with no sign-in here = %+v", v.Organizations)
	}
	text, err := runTerma(t, "teams")
	if err != nil || !strings.Contains(text, "[ ] Acme API") || !strings.Contains(text, "`terma teams --org \"Beta Labs\"`") {
		t.Errorf("the list as text (%v):\n%s", err, text)
	}
}

// Ticking is one step: its organization's sign-in, its policy, its key under a new
// enrollment in keys.v2.json, its bindings (--yes binds the listed repository), then
// routing.Update with Select.
func TestTeamsSelectTicksATeamWithItsKeyAndBindings(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	v, out, err := teamsJSON(t, "--select", "Acme API", "--yes")
	if err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	st := loadState(t)
	if len(st.Teams) != 1 {
		t.Fatalf("teams after ticking: %+v", st.Teams)
	}
	team := st.Teams[0]
	if team.TeamID != acmeAPI || team.OrganizationID != orgA().ID || team.AuthURL != f.srv.URL || team.Personal ||
		!slices.Equal(team.Policy.Repositories, []string{"github.com/acme/api"}) || team.StateAt(time.Now()) != routing.Active {
		t.Errorf("ticked team = %+v", team)
	}
	if b := st.Bindings["github.com/acme/api"]; b.Organization != orgA().ID {
		t.Errorf("the listed repository is not bound to Acme: %+v", st.Bindings)
	}
	key, _, err := keystore.GetTeam(testApp.dir, team.EnrollmentID)
	if err != nil || key == "" {
		t.Fatalf("no key under the enrollment: %q, %v", key, err)
	}
	if got := f.mintedNames; len(got) != 1 || !strings.HasSuffix(got[0], "/"+team.EnrollmentID) {
		t.Errorf("key names %v, want one named for the enrollment %s", got, team.EnrollmentID)
	}
	if _, row, _ := v.row(orgA().ID, acmeAPI); !row.Selected || row.EnrollmentID != team.EnrollmentID {
		t.Errorf("the list after ticking: %+v", row)
	}
	if len(v.Pending) != 0 {
		t.Errorf("pending after --yes: %+v", v.Pending)
	}
}

// Without anyone to ask and without --yes, what the ticked team needs stays to decide:
// pending, holding what it matches, never decided for the developer.
func TestTeamsSelectWithoutYesLeavesItsBindingsPending(t *testing.T) {
	migratedMachine(t, nil, orgA())
	v, out, err := teamsJSON(t, "--select", acmeAPI)
	if err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Bindings) != 0 || len(st.Teams) != 1 {
		t.Fatalf("state = %+v", st)
	}
	if len(v.Pending) != 1 || v.Pending[0].Key != "github.com/acme/api" || v.Pending[0].Organization != orgA().ID {
		t.Fatalf("pending = %+v", v.Pending)
	}
	text, _ := runTerma(t, "teams")
	if !strings.Contains(text, "`terma teams --bind github.com/acme/api=Acme`") {
		t.Errorf("the list does not say how to decide:\n%s", text)
	}
}

// A repository under an owner bound to another organization is never offered at ticking:
// the entry is skipped, and said.
func TestTeamsSelectSkipsAnEntryUnderAnOwnerBoundElsewhere(t *testing.T) {
	migratedMachine(t, func(_ *fakeAuth, s *routing.State) error { return s.Bind("github.com/acme", orgB().ID, false) }, orgA())
	v, out, err := teamsJSON(t, "--select", "Acme API", "--yes")
	if err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	st := loadState(t)
	if _, ok := st.Bindings["github.com/acme/api"]; ok || st.Bindings["github.com/acme"].Organization != orgB().ID {
		t.Errorf("bindings = %+v", st.Bindings)
	}
	if !slices.ContainsFunc(v.Warnings, func(w string) bool { return strings.Contains(w, "github.com/acme/api is skipped") }) {
		t.Errorf("warnings = %v", v.Warnings)
	}
}

// A team whose key cannot be made is not added, and nothing it decided stays.
func TestTeamsSelectChangesNothingWhenItsKeyCannotBeMade(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	f.mintDown = true
	_, out, err := teamsJSON(t, "--select", "Acme API", "--yes")
	if err == nil || !strings.Contains(err.Error(), "was not ticked") {
		t.Fatalf("teams --select = %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Teams) != 0 || len(st.Bindings) != 0 {
		t.Fatalf("a failed tick left %+v", st)
	}
}

// A team in an organization this machine has no sign-in for is not found by name; the
// error says how to sign in there.
func TestTeamsSelectInAnOrganizationWithoutASignInSaysHow(t *testing.T) {
	migratedMachine(t, nil, orgA())
	_, _, err := teamsJSON(t, "--select", "Beta Core")
	if err == nil || !strings.Contains(err.Error(), "Beta Labs") || !strings.Contains(err.Error(), "--org") {
		t.Fatalf("err = %v", err)
	}
}

// A shared organization's policy that collects everything collects nothing once ticked.
func TestTeamsSelectAnOrganizationsGlobalPolicyCollectsNothing(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	f.policyBodies[acmeAPI] = globalBody
	v, out, err := teamsJSON(t, "--select", "Acme API")
	if err != nil {
		t.Fatalf("teams --select: %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Teams) != 1 || st.Teams[0].Policy.Global() || len(st.Teams[0].Policy.Repositories) != 0 {
		t.Fatalf("state = %+v", st.Teams)
	}
	if !slices.ContainsFunc(v.Warnings, func(w string) bool { return strings.Contains(w, "collects nothing until an admin chooses") }) {
		t.Errorf("warnings = %v", v.Warnings)
	}
}

// Unticking ends the enrollment and deletes its key; bindings stay, and it says what then
// collects nowhere.
func TestTeamsDeselectSaysWhatThenCollectsNowhere(t *testing.T) {
	var enrollment string
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		var err error
		enrollment, err = s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/*"))
		if err != nil {
			return err
		}
		return s.Bind("github.com/acme", orgA().ID, false)
	}, orgA())
	if err := keystore.SetTeam(testApp.dir, enrollment, acmeAPI, testServerKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	v, out, err := teamsJSON(t, "--deselect", acmeAPI)
	if err != nil {
		t.Fatalf("teams --deselect: %v\n%s", err, out)
	}
	st := loadState(t)
	if len(st.Teams) != 0 || st.Bindings["github.com/acme"].Organization != orgA().ID {
		t.Fatalf("state after unticking = %+v", st)
	}
	if key, _, _ := keystore.GetTeam(testApp.dir, enrollment); key != "" {
		t.Error("the unticked enrollment's key is still on this machine")
	}
	if !slices.ContainsFunc(v.Warnings, func(w string) bool {
		return strings.Contains(w, "github.com/acme stays bound to Acme, so it collects nowhere") && strings.Contains(w, "--unbind github.com/acme")
	}) {
		t.Errorf("warnings = %v", v.Warnings)
	}
}

// --release removes only a suspended or refused team.
func TestTeamsReleaseRemovesOnlyASuspendedOrRefusedTeam(t *testing.T) {
	var active, refused string
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		var err error
		if active, err = s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api")); err != nil {
			return err
		}
		if refused, err = s.Select(stateTeam(f, orgA().ID, acmeWeb, false, "github.com/acme/web")); err != nil {
			return err
		}
		for _, n := range []int64{0, 1} {
			if _, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: refused, Epoch: n, Failure: routing.Terminal}); err != nil {
				return err
			}
		}
		return nil
	}, orgA())
	if _, _, err := teamsJSON(t, "--release", active); err == nil || !strings.Contains(err.Error(), "not suspended or refused") {
		t.Fatalf("releasing an active team = %v", err)
	}
	if _, out, err := teamsJSON(t, "--release", refused); err != nil {
		t.Fatalf("teams --release: %v\n%s", err, out)
	}
	if st := loadState(t); len(st.Teams) != 1 || st.Teams[0].EnrollmentID != active {
		t.Fatalf("teams after releasing = %+v", st.Teams)
	}
}

// A repository bound against its owner's decision names the conflict, and needs --yes
// without a terminal.
func TestTeamsBindAgainstTheOwnerNamesTheConflict(t *testing.T) {
	migratedMachine(t, func(_ *fakeAuth, s *routing.State) error { return s.Bind("github.com/acme", orgA().ID, false) }, orgA())
	_, _, err := teamsJSON(t, "--bind", "github.com/acme/api="+orgB().ID)
	if err == nil || !strings.Contains(err.Error(), "github.com/acme is bound to") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := loadState(t).Bindings["github.com/acme/api"]; ok {
		t.Fatal("the conflicting binding was made without confirmation")
	}
	if _, out, err := teamsJSON(t, "--bind", "https://GitHub.com/Acme/API.git="+orgB().ID, "--yes"); err != nil {
		t.Fatalf("teams --bind --yes: %v\n%s", err, out)
	}
	if b := loadState(t).Bindings["github.com/acme/api"]; b.Organization != orgB().ID {
		t.Fatalf("bindings = %+v", loadState(t).Bindings)
	}
}

// --refuse and --unbind decide and undecide; unbinding says what the key becomes.
func TestTeamsRefuseAndUnbind(t *testing.T) {
	migratedMachine(t, func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
		return err
	}, orgA())
	if _, out, err := teamsJSON(t, "--refuse", "github.com/acme/api"); err != nil {
		t.Fatalf("teams --refuse: %v\n%s", err, out)
	}
	if b := loadState(t).Bindings["github.com/acme/api"]; !b.Refused {
		t.Fatalf("bindings = %+v", loadState(t).Bindings)
	}
	v, out, err := teamsJSON(t, "--unbind", "github.com/acme/api")
	if err != nil {
		t.Fatalf("teams --unbind: %v\n%s", err, out)
	}
	if len(loadState(t).Bindings) != 0 || !slices.ContainsFunc(v.Changes, func(c string) bool { return strings.Contains(c, "it is now pending") }) {
		t.Fatalf("changes = %v", v.Changes)
	}
	if _, _, err := teamsJSON(t, "--bind", "github.com/acme/*=Acme"); err == nil {
		t.Fatal("a wildcard was taken as a binding key")
	}
}

// Before migration, `terma teams` stops the relay, starts this executable, and waits for
// the migration to commit; with no commit it fails and nothing changes.
func TestTeamsMigratesTheMachineFirst(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, orgA())); err != nil {
		t.Fatal(err)
	}
	prevSpawn, prevWait := testApp.spawnRelay, testApp.migrationWait
	t.Cleanup(func() { testApp.spawnRelay, testApp.migrationWait = prevSpawn, prevWait })
	testApp.migrationWait = 300 * time.Millisecond
	started := 0
	testApp.spawnRelay = func(string, string, string) { started++ }
	if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("teams with no migration = %v", err)
	}
	if started != 1 || config.Migrated(testApp.dir) {
		t.Fatalf("relay starts %d, migrated %v", started, config.Migrated(testApp.dir))
	}
	// The relay this terma starts migrates: the v2 index, then the committed state.
	testApp.spawnRelay = func(string, string, string) {
		started++
		if err := auth.CopyToV2(testApp.dir); err != nil {
			t.Error(err)
		}
		if _, err := routing.Create(testApp.stateDir, config.DefaultProfile, func(s *routing.State) error {
			s.Migration = &routing.Migration{Phase: routing.Committed}
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	v, out, err := teamsJSON(t)
	if err != nil {
		t.Fatalf("teams after the migration: %v\n%s", err, out)
	}
	if started != 2 || !config.Migrated(testApp.dir) || len(v.Organizations) == 0 {
		t.Fatalf("relay starts %d, list %+v", started, v)
	}
	// Once migrated, it never restarts the relay again.
	if _, _, err := teamsJSON(t); err != nil || started != 2 {
		t.Fatalf("teams on a migrated machine: %v, relay starts %d", err, started)
	}
}

// A machine signed in with a team server key stays on its one team.
func TestTeamsRefusesAServerKeySignIn(t *testing.T) {
	migratedMachine(t, nil, orgA())
	t.Setenv("TERMA_API_KEY", testServerKey)
	if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "server key") {
		t.Fatalf("err = %v", err)
	}
}

// A migrated machine whose routing state is unreadable changes nothing and says so.
func TestTeamsOnAnUnreadableRoutingStateFails(t *testing.T) {
	migratedMachine(t, nil, orgA())
	path, _ := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := teamsJSON(t, "--select", "Acme API"); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("err = %v", err)
	}
}
