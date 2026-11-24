package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

const (
	teamsAcmeWeb      = "aaaaaaaa-0000-4000-8000-000000000001"
	acmeAPI           = "aaaaaaaa-0000-4000-8000-000000000002"
	betaCore          = "bbbbbbbb-0000-4000-8000-000000000001"
	teamsPersonalOrg  = "99999999-9999-4999-8999-999999999999"
	teamsPersonalTeam = "pppppppp-0000-4000-8000-000000000001"
)

// policyBody is a 1.1 policy response listing repos, as /v1/policy answers it.
func policyBody(repos ...string) string {
	list, _ := json.Marshal(repos)
	return `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"per_repository":{"repositories":` +
		string(list) + `}}},"revision":3,"updated_at":"2026-11-01T00:00:00Z"}`
}

// wildcardPolicyBody is a 2.0 policy response listing wildcard/exact entries.
func wildcardPolicyBody(repos ...string) string {
	list, _ := json.Marshal(repos)
	return `{"policy":{"version":"2.0","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"per_repository":{"repositories":` +
		string(list) + `}}},"revision":4,"updated_at":"2026-11-01T00:00:00Z"}`
}

// globalBody is a policy response that collects every session.
const globalBody = `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":true,"global":{}}},"revision":2,"updated_at":"2026-11-01T00:00:00Z"}`

func orgPersonal() organization {
	return organization{ID: teamsPersonalOrg, Name: "Personal", Type: "personal"}
}

// routedMachine sets up a profile signed in as user (default "u-1") with a single user
// token and an account-stamped routing.State seeded by seed.
func routedMachine(t *testing.T, user string, seed func(*fakeAuth, *routing.State) error) (*fakeAuth, string) {
	t.Helper()
	f := newFakeAuth(t)
	authSandbox(t, f)
	sandboxMachine(t)
	t.Setenv("TERMA_POLICY_STUB", "")
	f.policyBodies = map[string]string{
		teamsAcmeWeb:      policyBody("github.com/acme/web"),
		acmeAPI:           policyBody("github.com/acme/api"),
		betaCore:          policyBody("github.com/beta/core"),
		teamsPersonalTeam: globalBody,
	}
	if user == "" {
		user = "u-1"
	}
	cred := storedSessionOf(f, user)
	if err := saveSignIn(testApp.dir, cred); err != nil {
		t.Fatal(err)
	}
	account := cred.Account()
	if _, err := routing.Create(testApp.stateDir, config.DefaultProfile, account, func(s *routing.State) error {
		if seed == nil {
			return nil
		}
		return seed(f, s)
	}); err != nil {
		t.Fatal(err)
	}
	return f, account
}

// stateTeam constructs a routing.Team for team in org with a validated policy.
func stateTeam(f *fakeAuth, org, team string, personal bool, repos ...string) routing.Team {
	pol := config.Policy{
		Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: time.Now().UTC(),
		OrganizationID: org, AuthURL: f.srv.URL, TeamID: team, GitHooks: true,
	}
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
		t.Fatalf("routing state: ok=%v, err=%v", ok, err)
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

// Invariant 4: no --authorize, no --org re-auth, no --no-browser flags exist on terma teams.
func TestTeamsHasNoAuthorizeOrOrgReauthFlags(t *testing.T) {
	cmd := commandNamed(testApp.NewRootCommand(), "teams")
	if cmd == nil {
		t.Fatal("teams command not registered")
	}
	for _, forbidden := range []string{"authorize", "org", "no-browser"} {
		if fl := cmd.Flags().Lookup(forbidden); fl != nil {
			t.Errorf("terma teams exposes removed flag --%s", forbidden)
		}
	}
}

func TestTeamsHonorsPersistentOutputFlag(t *testing.T) {
	routedMachine(t, "alice", nil)
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) {
		t.Fatal("interactive checklist must not open under --output json")
		return nil, nil
	}

	stdout, stderr, err := termaRun{}.exec(t, "teams", "--output", "json")
	if err != nil {
		t.Fatalf("teams --output json: %v\n%s\n%s", err, stdout, stderr)
	}
	var v teamsView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("teams --output json emitted non-JSON (%v):\n%s", err, stdout)
	}
	if _, _, ok := v.row(orgA().ID, acmeAPI); !ok {
		t.Fatalf("teams --output json missing Acme API: %+v", v.Organizations)
	}
	for _, bad := range []string{"yaml", "csv", "bogus"} {
		_, _, err := termaRun{}.exec(t, "teams", "--output", bad)
		if err == nil {
			t.Errorf("teams --output %s succeeded, want error", bad)
		}
	}
}

// Invariant 1 & 5: single user token lists all organizations and teams immediately without
// per-org authorization; 403 PROJECT_NOT_FOUND hides deleted teams cleanly while bare 403
// stays listed as unreadable.
func TestTeamsListsAllOrgsOnSingleUserTokenAndHidesProjectNotFound(t *testing.T) {
	f, _ := routedMachine(t, "alice", nil)
	f.orgs = []organization{orgPersonal(), orgA(), orgB()}
	f.projects = map[string][]project{
		teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
		orgA().ID:        projectsIn(orgA().ID),
		orgB().ID:        projectsIn(orgB().ID),
	}
	f.goneTeams = map[string]bool{teamsAcmeWeb: true}
	f.forbiddenTeams = map[string]bool{acmeAPI: true}

	v, out, err := teamsJSON(t)
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	// Personal is always ordered last regardless of API order.
	if len(v.Organizations) != 3 || v.Organizations[2].ID != teamsPersonalOrg {
		t.Fatalf("organizations = %+v, want Personal last", v.Organizations)
	}
	// 403 PROJECT_NOT_FOUND hides deleted team.
	if _, _, ok := v.row(orgA().ID, teamsAcmeWeb); ok {
		t.Error("team returning 403 PROJECT_NOT_FOUND should be hidden")
	}
	// Bare 403 stays listed as unreadable.
	if _, api, ok := v.row(orgA().ID, acmeAPI); !ok || api.Unreadable == "" {
		t.Errorf("team returning bare 403 = %+v (listed=%v), want unreadable", api, ok)
	}
	// Beta Labs and Personal are listed immediately on the same single user token.
	if _, beta, ok := v.row(orgB().ID, betaCore); !ok || beta.Collects != "github.com/beta/core" {
		t.Errorf("Beta Core = %+v (listed=%v)", beta, ok)
	}
	if _, pers, ok := v.row(teamsPersonalOrg, teamsPersonalTeam); !ok || pers.Collects != personalCollects {
		t.Errorf("Personal = %+v (listed=%v)", pers, ok)
	}

	// Text output lists every org and has no --authorize prompt.
	f.forbiddenTeams = nil
	text, err := runTerma(t, "teams")
	if err != nil {
		t.Fatalf("teams text: %v\n%s", err, text)
	}
	if !strings.Contains(text, "[ ] Acme API") || !strings.Contains(text, "[ ] Beta Core") || !strings.Contains(text, personalCollects) {
		t.Errorf("unexpected text output:\n%s", text)
	}
	if strings.Contains(text, "--authorize") || strings.Contains(text, "--org") {
		t.Errorf("text output mentions removed flags:\n%s", text)
	}
}

// Invariant 1 (concurrency & deterministic order): single user token lists orgs/teams
// concurrently capped at listLimit and preserves listing order even when earlier requests
// finish last.
func TestTeamsListsConcurrentlyBoundedAndInOrder(t *testing.T) {
	const nOrgs, nTeams = 6, 3
	orgs := make([]organization, 0, nOrgs)
	projects := map[string][]project{}
	for i := range nOrgs {
		o := organization{ID: fmt.Sprintf("cccccccc-0000-4000-8000-%012d", i), Name: fmt.Sprintf("Org %d", i), Role: "member"}
		orgs = append(orgs, o)
		for j := range nTeams {
			projects[o.ID] = append(projects[o.ID], project{
				ID: fmt.Sprintf("dddddddd-%04d-4000-8000-%012d", i, j), Name: fmt.Sprintf("Team %d.%d", i, j), OrganizationID: o.ID,
			})
		}
	}
	f, _ := routedMachine(t, "alice", nil)
	f.orgs, f.projects = orgs, projects
	for i, o := range orgs {
		for j, p := range projects[o.ID] {
			f.policyBodies[p.ID] = policyBody(fmt.Sprintf("github.com/o%d/t%d", i, j))
		}
	}
	var inFlight, peak atomic.Int32
	f.slow = func(r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for m := peak.Load(); n > m && !peak.CompareAndSwap(m, n); m = peak.Load() {
		}
		delay := time.Millisecond
		if team := r.URL.Query().Get("project_id"); team != "" {
			var i, j int
			_, _ = fmt.Sscanf(team, "dddddddd-%04d-4000-8000-%012d", &i, &j)
			delay = time.Duration(nTeams-j) * 3 * time.Millisecond
		} else if org := r.URL.Query().Get("organization_id"); org != "" {
			var i int
			_, _ = fmt.Sscanf(org, "cccccccc-0000-4000-8000-%012d", &i)
			delay = time.Duration(nOrgs-i) * 3 * time.Millisecond
		}
		time.Sleep(delay)
	}

	v, out, err := teamsJSON(t)
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	if peak.Load() < 2 || peak.Load() > listLimit {
		t.Errorf("peak concurrent requests = %d, want between 2 and %d", peak.Load(), listLimit)
	}
	if len(v.Organizations) != nOrgs {
		t.Fatalf("got %d organizations, want %d", len(v.Organizations), nOrgs)
	}
	for i, o := range v.Organizations {
		if o.ID != orgs[i].ID || len(o.Teams) != nTeams {
			t.Fatalf("org[%d] = %+v, want %s with %d teams", i, o, orgs[i].ID, nTeams)
		}
		for j, tm := range o.Teams {
			want := projects[o.ID][j]
			if tm.ID != want.ID || tm.Collects != fmt.Sprintf("github.com/o%d/t%d", i, j) {
				t.Errorf("org[%d].team[%d] = %+v, want %s", i, j, tm, want.ID)
			}
		}
	}
}

// Invariant 6: 403 ORG_NOT_ACCESSIBLE on /v1/projects or /v1/policy annotates the
// inaccessible org without failing listing of other orgs or Personal.
func TestTeamsAnnotatesOrgNotAccessibleWithoutFailingOthers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		projectsDenied bool
	}{
		{name: "projects endpoint returns ORG_NOT_ACCESSIBLE", projectsDenied: true},
		{name: "policy endpoint returns ORG_NOT_ACCESSIBLE", projectsDenied: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := routedMachine(t, "alice", nil)
			f.orgs = []organization{orgA(), orgB(), orgPersonal()}
			f.projects = map[string][]project{
				orgA().ID:        projectsIn(orgA().ID),
				orgB().ID:        projectsIn(orgB().ID),
				teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
			}
			if tc.projectsDenied {
				f.orgGone = orgB().ID
			} else {
				f.orgGonePolicyOnly = orgB().ID
			}

			v, out, err := teamsJSON(t)
			if err != nil {
				t.Fatalf("teams: %v\n%s", err, out)
			}
			if v.Unlisted != "" {
				t.Fatalf("unexpected top-level Unlisted error: %q", v.Unlisted)
			}
			// Acme and Personal list normally.
			if _, api, ok := v.row(orgA().ID, acmeAPI); !ok || api.Collects != "github.com/acme/api" {
				t.Errorf("Acme API = %+v (ok=%v)", api, ok)
			}
			if _, pers, ok := v.row(teamsPersonalOrg, teamsPersonalTeam); !ok || pers.Collects != personalCollects {
				t.Errorf("Personal = %+v (ok=%v)", pers, ok)
			}
			// Beta Labs is annotated as inaccessible.
			bi := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == orgB().ID })
			if bi < 0 || !v.Organizations[bi].Inaccessible || !strings.Contains(v.Organizations[bi].Note, "no longer have access") {
				t.Fatalf("Beta Labs row = %+v, want Inaccessible with clear note", v.Organizations[bi])
			}
			// Text rendering prints the annotation while showing Acme and Personal.
			text, err := runTerma(t, "teams")
			if err != nil {
				t.Fatalf("teams text: %v\n%s", err, text)
			}
			if !strings.Contains(text, orgInaccessibleNote) || !strings.Contains(text, "Acme API") || !strings.Contains(text, "Personal") {
				t.Errorf("unexpected text output:\n%s", text)
			}
			// Attempting to tick a team or bind to the inaccessible org (by name or UUID) fails cleanly.
			if _, _, err := teamsJSON(t, "--select", betaCore); err == nil {
				t.Error("selecting a team in an ORG_NOT_ACCESSIBLE org should fail")
			}
			if _, _, err := teamsJSON(t, "--bind", "github.com/beta/core="+orgB().ID); err == nil || !strings.Contains(err.Error(), "no longer have access") {
				t.Errorf("binding to ORG_NOT_ACCESSIBLE org by UUID err = %v, want inaccessible rejection", err)
			}
		})
	}
}

func TestTeamsPreservesFreshGlobalPolicyOverStaleState(t *testing.T) {
	f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
		return err
	})
	// Server policy switched from per-repository mode (git_hooks=true) to global mode (git_hooks=false).
	f.policyBodies[acmeAPI] = `{"policy":{"version":"1.1","terma":{"capture":{"exclude_prompts":false,"exclude_tool_content":false},"git_hooks":false,"global":{}}},"revision":4,"updated_at":"2026-11-02T00:00:00Z"}`

	v, out, err := teamsJSON(t)
	if err != nil {
		t.Fatalf("teams: %v\n%s", err, out)
	}
	_, row, ok := v.row(orgA().ID, acmeAPI)
	if !ok {
		t.Fatal("Acme API row missing")
	}
	if len(row.Entries) != 0 || row.GitHooks || row.Collects != "nothing until an admin chooses repositories" {
		t.Fatalf("fresh global policy overwritten by stale state: %+v", row)
	}
}

func TestTeamsSelectRefreshesViewWhenPolicyChangesBetweenListAndTick(t *testing.T) {
	f, _ := routedMachine(t, "alice", nil)
	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		// While the checklist is open, an admin updates Acme API's policy from api -> payments.
		f.policyBodies[acmeAPI] = policyBody("github.com/acme/payments")
		items := slices.Clone(form.Items)
		for i := range items {
			if items[i].Label == "Acme API" {
				items[i].Selected = true
			}
		}
		return items, nil
	}

	stdout, stderr, err := termaRun{}.exec(t, "teams", "--yes")
	if err != nil {
		t.Fatalf("teams --yes: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "github.com/acme/payments") || strings.Contains(stdout, "github.com/acme/api") {
		t.Fatalf("returned view still has stale policy:\n%s", stdout)
	}
}

// Invariant 2 (Council 1): an open interactive `terma teams` session captures
// (Account, StateID, ConsentGeneration) before prompting. A concurrent consent edit or a
// cross-account switch (Alice -> Bob) rejects Alice's confirm without replaying stale
// consent, whereas a background policy refresh (Generation only) does not cause a
// spurious conflict.
func TestTeamsCASRejectsStaleConsentAndAccountSwitch(t *testing.T) {
	t.Run("stale consent generation rejects Alice's open session", func(t *testing.T) {
		_, aliceAccount := routedMachine(t, "alice", nil)
		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			// While Alice's checklist is open, another `terma teams` command binds a repo
			// (advancing ConsentGeneration).
			cur := loadState(t)
			if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, cur.ExpectConsent(), func(s *routing.State) error {
				return s.Bind("github.com/beta/core", orgB().ID, false)
			}); err != nil {
				t.Fatalf("concurrent bind: %v", err)
			}
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Acme API" {
					items[i].Selected = true
				}
			}
			return items, nil
		}

		_, _, err := termaRun{}.exec(t, "teams", "--yes")
		if !errors.Is(err, routing.ErrStaleGeneration) {
			t.Fatalf("got err = %v, want routing.ErrStaleGeneration", err)
		}
		st := loadState(t)
		if st.Account != aliceAccount || len(st.Teams) != 0 {
			t.Fatalf("stale consent was replayed into state: %+v", st)
		}
		if st.Bindings["github.com/beta/core"].Organization != orgB().ID || st.Bindings["github.com/acme/api"].Organization != "" {
			t.Fatalf("bindings corrupted on CAS conflict: %+v", st.Bindings)
		}
	})

	t.Run("cross-account switch to Bob rejects Alice's open session", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
			return err
		})
		bobCred := storedSessionOf(f, "bob")
		bobAccount := bobCred.Account()

		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			// While Alice has `terma teams` open, Bob runs `terma setup`, replacing the
			// profile's routing state with Bob's account and Bob's own team.
			if _, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, bobAccount, func(s *routing.State) error {
				_, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/beta/core"))
				return err
			}); err != nil {
				t.Fatalf("bob replace: %v", err)
			}
			// Alice unticks Acme API (no API fetch needed — exercises the CAS write gate directly).
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Acme API" {
					items[i].Selected = false
				}
			}
			return items, nil
		}

		_, _, err := termaRun{}.exec(t, "teams", "--yes")
		if !errors.Is(err, routing.ErrAccountMismatch) {
			t.Fatalf("got err = %v, want routing.ErrAccountMismatch", err)
		}
		st := loadState(t)
		if st.Account != bobAccount || len(st.Teams) != 1 || st.Teams[0].TeamID != betaCore {
			t.Fatalf("Bob's routing state was modified by Alice's stale session: %+v", st)
		}
	})

	t.Run("background policy refresh does not break Alice's open session", func(t *testing.T) {
		var enrID string
		f, aliceAccount := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			var err error
			enrID, err = s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
			return err
		})

		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			// Relay daemon runs a background policy refresh (advances Generation, NOT ConsentGeneration).
			pol := config.Policy{
				Mode: config.ModeRepo, Repositories: []string{"github.com/acme/web", "github.com/acme/web2"},
				Revision: 5, FetchedAt: time.Now().UTC(), OrganizationID: orgA().ID, AuthURL: f.srv.URL, TeamID: teamsAcmeWeb,
			}
			if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, routing.Expect{Account: aliceAccount}, func(s *routing.State) error {
				_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: enrID, Epoch: 0, Policy: &pol})
				return err
			}); err != nil {
				t.Fatalf("background refresh: %v", err)
			}
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Acme API" {
					items[i].Selected = true
				}
			}
			return items, nil
		}

		stdout, stderr, err := termaRun{}.exec(t, "teams", "--yes")
		if err != nil {
			t.Fatalf("teams across background refresh failed: %v\n%s\n%s", err, stdout, stderr)
		}
		st := loadState(t)
		if len(st.Teams) != 2 {
			t.Fatalf("teams = %+v, want 2", st.Teams)
		}
		// Refreshed policy on teamsAcmeWeb was preserved!
		tm, ok := st.Team(enrID)
		if !ok || tm.Policy.Revision != 5 || len(tm.Policy.Repositories) != 2 {
			t.Fatalf("background refreshed policy lost: %+v", tm)
		}
	})

	t.Run("startup refuses when live sign-in belongs to another account", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
		// Replace stored sign-in with Bob while routing state still belongs to Alice.
		if err := saveSignIn(testApp.dir, storedSessionOf(f, "bob")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme/api="+orgA().ID); !errors.Is(err, routing.ErrAccountMismatch) {
			t.Fatalf("got err = %v, want routing.ErrAccountMismatch", err)
		}
		if st := loadState(t); len(st.Bindings) != 0 {
			t.Fatalf("binding written despite account mismatch: %+v", st.Bindings)
		}
	})
}

// Invariant 3: refuses server-key profiles via TERMA_API_KEY, ServerKeySignIn config, or
// routing.State.Account == no_person.
func TestTeamsRefusesServerKeyProfiles(t *testing.T) {
	t.Run("TERMA_API_KEY env", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		t.Setenv("TERMA_API_KEY", testServerKey)
		if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "server key") {
			t.Fatalf("err = %v, want server key refusal", err)
		}
	})

	t.Run("ServerKeySignIn profile flag", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
			p.ServerKeySignIn = true
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "server key") {
			t.Fatalf("err = %v, want server key refusal", err)
		}
	})

	t.Run("no_person routing state", func(t *testing.T) {
		f := newFakeAuth(t)
		authSandbox(t, f)
		sandboxMachine(t)
		t.Setenv("TERMA_POLICY_STUB", "")
		if _, err := routing.Create(testApp.stateDir, config.DefaultProfile, sendas.NoPersonLane, func(s *routing.State) error {
			_, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "server key") {
			t.Fatalf("err = %v, want server key refusal", err)
		}
	})
}

// Invariant 4 & 5: ticking a team on one user token mints no server keys, stores its
// policy file, handles 2.x wildcard policies and pending bindings, downgrades a shared
// org's global policy, and rejects a team deleted (403 PROJECT_NOT_FOUND) before commit.
func TestTeamsSelectWildcardPendingGlobalAndNoKeyMinting(t *testing.T) {
	t.Run("select wildcard with --yes binds owner and mints no key", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
		f.policyBodies[acmeAPI] = wildcardPolicyBody("github.com/acme/*")
		v, out, err := teamsJSON(t, "--select", "Acme API", "--yes")
		if err != nil {
			t.Fatalf("teams --select: %v\n%s", err, out)
		}
		st := loadState(t)
		if len(st.Teams) != 1 || !st.Teams[0].Policy.Wildcards || !slices.Equal(st.Teams[0].Policy.Repositories, []string{"github.com/acme/*"}) {
			t.Fatalf("ticked wildcard team = %+v", st.Teams)
		}
		if b := st.Bindings["github.com/acme"]; b.Organization != orgA().ID {
			t.Errorf("owner not bound to Acme: %+v", st.Bindings)
		}
		if key, err := keystore.Get(testApp.dir, acmeAPI); err != nil || key != "" {
			t.Errorf("keystore has minted key %q (err=%v), want none", key, err)
		}
		if pol, ok, err := config.ReadPolicy(testApp.stateDir, acmeAPI); err != nil || !ok || !pol.Wildcards {
			t.Errorf("stored policy file = (%+v, %v, %v)", pol, ok, err)
		}
		if len(v.Pending) != 0 {
			t.Errorf("unexpected pending bindings: %+v", v.Pending)
		}
	})

	t.Run("select without --yes leaves bindings pending", func(t *testing.T) {
		routedMachine(t, "alice", nil)
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
		text, err := runTerma(t, "teams")
		if err != nil || !strings.Contains(text, "`terma teams --bind github.com/acme/api=Acme`") {
			t.Errorf("text output missing bind instruction (%v):\n%s", err, text)
		}
	})

	t.Run("select skips entry under owner bound elsewhere", func(t *testing.T) {
		routedMachine(t, "alice", func(_ *fakeAuth, s *routing.State) error {
			return s.Bind("github.com/acme", orgB().ID, false)
		})
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
	})

	t.Run("shared org global policy is downgraded to empty repo mode", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
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
	})
}

// Invariant 7: deselect keeps bindings so bound repos never spill into Personal, and warns
// clearly. --release removes only Suspended or Refused teams.
func TestTeamsDeselectKeepsBindingsAndReleaseRequiresBlockedState(t *testing.T) {
	t.Run("deselect keeps bindings and warns", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/*")); err != nil {
				return err
			}
			if _, err := s.Select(stateTeam(f, teamsPersonalOrg, teamsPersonalTeam, true)); err != nil {
				return err
			}
			return s.Bind("github.com/acme", orgA().ID, false)
		})
		f.orgs = []organization{orgA(), orgPersonal()}
		f.projects = map[string][]project{
			orgA().ID:        projectsIn(orgA().ID),
			teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
		}

		v, out, err := teamsJSON(t, "--deselect", acmeAPI)
		if err != nil {
			t.Fatalf("teams --deselect: %v\n%s", err, out)
		}
		st := loadState(t)
		if len(st.Teams) != 1 || !st.Teams[0].Personal || st.Bindings["github.com/acme"].Organization != orgA().ID {
			t.Fatalf("state after deselect = %+v", st)
		}
		if !slices.ContainsFunc(v.Warnings, func(w string) bool {
			return strings.Contains(w, "github.com/acme stays bound to Acme, so it collects nowhere") && strings.Contains(w, "--unbind github.com/acme")
		}) {
			t.Errorf("missing orphaned binding warning: %v", v.Warnings)
		}
		if !slices.ContainsFunc(v.Warnings, func(w string) bool {
			return strings.Contains(w, "unknown never goes to personal")
		}) {
			t.Errorf("missing personal unknown-location warning: %v", v.Warnings)
		}
	})

	t.Run("release removes only suspended or refused teams", func(t *testing.T) {
		var active, refused string
		routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			var err error
			if active, err = s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api")); err != nil {
				return err
			}
			if refused, err = s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web")); err != nil {
				return err
			}
			for _, epoch := range []int64{0, 1} {
				if _, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: refused, Epoch: epoch, Failure: routing.Terminal}); err != nil {
					return err
				}
			}
			return nil
		})
		if _, _, err := teamsJSON(t, "--release", active); err == nil || !strings.Contains(err.Error(), "not suspended or refused") {
			t.Fatalf("releasing an active team = %v", err)
		}
		if _, out, err := teamsJSON(t, "--release", refused); err != nil {
			t.Fatalf("teams --release: %v\n%s", err, out)
		}
		if st := loadState(t); len(st.Teams) != 1 || st.Teams[0].EnrollmentID != active {
			t.Fatalf("teams after release = %+v", st.Teams)
		}
	})
}

// Bindings: conflict against owner requires --yes without terminal; --refuse and --unbind
// work; wildcards and personal org bindings are refused.
func TestTeamsBindRefuseAndUnbind(t *testing.T) {
	routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
		if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api")); err != nil {
			return err
		}
		return s.Bind("github.com/acme", orgA().ID, false)
	})
	_, _, err := teamsJSON(t, "--bind", "github.com/acme/api="+orgB().ID)
	if err == nil || !strings.Contains(err.Error(), "github.com/acme is bound to") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("conflict without --yes = %v", err)
	}
	if _, ok := loadState(t).Bindings["github.com/acme/api"]; ok {
		t.Fatal("conflicting binding written without confirmation")
	}
	if _, out, err := teamsJSON(t, "--bind", "https://GitHub.com/Acme/API.git="+orgB().ID, "--yes"); err != nil {
		t.Fatalf("teams --bind --yes: %v\n%s", err, out)
	}
	if b := loadState(t).Bindings["github.com/acme/api"]; b.Organization != orgB().ID {
		t.Fatalf("bindings = %+v", loadState(t).Bindings)
	}
	if _, out, err := teamsJSON(t, "--refuse", "github.com/acme/api"); err != nil {
		t.Fatalf("teams --refuse: %v\n%s", err, out)
	}
	if !loadState(t).Bindings["github.com/acme/api"].Refused {
		t.Fatalf("bindings after refuse = %+v", loadState(t).Bindings)
	}
	if _, out, err := teamsJSON(t, "--unbind", "github.com/acme"); err != nil {
		t.Fatalf("teams --unbind owner: %v\n%s", err, out)
	}
	v, out, err := teamsJSON(t, "--unbind", "github.com/acme/api")
	if err != nil {
		t.Fatalf("teams --unbind repo: %v\n%s", err, out)
	}
	if len(loadState(t).Bindings) != 0 || !slices.ContainsFunc(v.Changes, func(c string) bool { return strings.Contains(c, "it is now pending") }) {
		t.Fatalf("changes = %v, bindings = %+v", v.Changes, loadState(t).Bindings)
	}
	if _, _, err := teamsJSON(t, "--bind", "github.com/acme/*=Acme"); err == nil {
		t.Fatal("wildcard was accepted as a binding key")
	}
}

// SignInNeeded points to `terma setup` (not removed --org), and missing or unreadable
// routing state fails closed.
func TestTeamsSignInNeededMessageAndInvalidStateFailure(t *testing.T) {
	t.Run("SignInNeeded recommends terma setup", func(t *testing.T) {
		routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			id, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/api"))
			if err != nil {
				return err
			}
			_, err = s.ApplyRefresh(routing.Refresh{EnrollmentID: id, Epoch: 0, Failure: routing.SignIn})
			return err
		})
		text, err := runTerma(t, "teams")
		if err != nil {
			t.Fatalf("teams: %v\n%s", err, text)
		}
		if !strings.Contains(text, "run `terma setup` to sign in again") || strings.Contains(text, "--org") {
			t.Errorf("unexpected SignInNeeded wording:\n%s", text)
		}
	})

	t.Run("missing routing state fails closed", func(t *testing.T) {
		f := newFakeAuth(t)
		authSandbox(t, f)
		sandboxMachine(t)
		t.Setenv("TERMA_POLICY_STUB", "")
		if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := teamsJSON(t); err == nil || !strings.Contains(err.Error(), "no routing state") {
			t.Fatalf("err = %v, want missing routing state error", err)
		}
	})

	t.Run("unreadable routing state fails closed", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		path, _ := routing.StatePath(testApp.stateDir, config.DefaultProfile)
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := teamsJSON(t, "--select", "Acme API"); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Fatalf("err = %v, want unreadable routing state error", err)
		}
	})

	t.Run("interactive checklist cancellation changes nothing", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) {
			return nil, prompt.ErrCancelled
		}
		out, err := runTerma(t, "teams")
		if err != nil || !strings.Contains(out, "Cancelled. Nothing was changed.") {
			t.Fatalf("cancel = (%q, %v)", out, err)
		}
	})

	t.Run("stale cached org name resolves against live organizations", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		// Seed a stale cache claiming orgB is named "Acme" while live API says orgA is "Acme".
		stale := teamNames{Orgs: map[string]string{}, Teams: map[string]string{}}
		stale.org(orgB().ID, "Acme")
		stale.save(testApp.stateDir, config.DefaultProfile)
		if _, out, err := teamsJSON(t, "--bind", "github.com/acme/api=Acme"); err != nil {
			t.Fatalf("teams --bind: %v\n%s", err, out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if b := st.Bindings["github.com/acme/api"]; b.Organization != orgA().ID {
			t.Fatalf("bound org = %q, want live %q (not stale cached %q)", b.Organization, orgA().ID, orgB().ID)
		}
	})

	t.Run("duplicate team names across organizations require an id", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
		dupA := project{ID: "dddddddd-0000-4000-8000-000000000001", Name: "Platform"}
		dupB := project{ID: "dddddddd-0000-4000-8000-000000000002", Name: "Platform"}
		f.projects = map[string][]project{
			orgA().ID: {dupA},
			orgB().ID: {dupB},
		}
		f.policyBodies[dupA.ID] = policyBody("github.com/acme/api")
		f.policyBodies[dupB.ID] = policyBody("github.com/beta/core")

		if _, _, err := teamsJSON(t, "--select", "Platform", "--yes"); err == nil || !strings.Contains(err.Error(), "matches several") {
			t.Fatalf("duplicate --select err = %v, want ambiguity error", err)
		}
		if _, out, err := teamsJSON(t, "--select", dupB.ID, "--yes"); err != nil {
			t.Fatalf("--select by id: %v\n%s", err, out)
		}
		st, _, _ := routing.Load(testApp.stateDir, config.DefaultProfile)
		if len(st.Teams) != 1 || st.Teams[0].TeamID != dupB.ID {
			t.Fatalf("selected teams = %+v, want only %s", st.Teams, dupB.ID)
		}
	})

	t.Run("deselect preserves shared team policy file", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		if _, out, err := teamsJSON(t, "--select", "Acme API", "--yes"); err != nil {
			t.Fatalf("select: %v\n%s", err, out)
		}
		path, err := config.PolicyPath(testApp.stateDir, acmeAPI)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("policy file not stored on select: %v", err)
		}
		if _, out, err := teamsJSON(t, "--deselect", "Acme API"); err != nil {
			t.Fatalf("deselect: %v\n%s", err, out)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("deselect deleted shared policy file %s: %v", path, err)
		}
	})

	t.Run("quoteArg prevents shell expansion in suggested commands", func(t *testing.T) {
		cases := []struct {
			in   string
			want string
		}{
			{"Acme", "Acme"},
			{"github.com/acme/api=Acme", "github.com/acme/api=Acme"},
			{"Team $(printf pwn)", "'Team $(printf pwn)'"},
			{"O'Reilly", `'O'\''Reilly'`},
			{"", "''"},
		}
		for _, tc := range cases {
			if got := quoteArg(tc.in); got != tc.want {
				t.Errorf("quoteArg(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("refusing an owner warns about existing child repository exceptions", func(t *testing.T) {
		routedMachine(t, "alice", func(_ *fakeAuth, s *routing.State) error {
			return s.Bind("github.com/acme/api", orgA().ID, false)
		})
		v, out, err := teamsJSON(t, "--refuse", "github.com/acme")
		if err != nil {
			t.Fatalf("--refuse owner: %v\n%s", err, out)
		}
		if !slices.ContainsFunc(v.Warnings, func(w string) bool {
			return strings.Contains(w, "kept github.com/acme/api") && strings.Contains(w, "under github.com/acme")
		}) {
			t.Fatalf("warnings = %v, want warning about kept child exception", v.Warnings)
		}
	})

	t.Run("binding to personal organization is rejected by name and id even when unselected", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
		hexPersonal := "99999999-aaaa-4999-8999-999999999999"
		f.orgs = []organization{orgA(), orgB(), {ID: hexPersonal, Name: "Personal", Type: "personal"}}
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme/api=Personal"); err == nil || !strings.Contains(err.Error(), "personal") {
			t.Fatalf("bind Personal by name err = %v, want personal rejection", err)
		}
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme/api="+hexPersonal); err == nil || !strings.Contains(err.Error(), "personal") {
			t.Fatalf("bind Personal by id err = %v, want personal rejection", err)
		}
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme/api="+strings.ToUpper(hexPersonal)); err == nil || !strings.Contains(err.Error(), "personal") {
			t.Fatalf("bind Personal by uppercase id err = %v, want personal rejection", err)
		}
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme/api=00000000-0000-4000-8000-000000000099"); err == nil || !strings.Contains(err.Error(), "not one of your organizations") {
			t.Fatalf("bind unknown UUID err = %v, want unknown-org rejection", err)
		}
	})

	t.Run("unbinding a repository reports inherited owner decision", func(t *testing.T) {
		routedMachine(t, "alice", func(_ *fakeAuth, s *routing.State) error {
			if err := s.Bind("github.com/acme", orgA().ID, false); err != nil {
				return err
			}
			return s.Bind("github.com/acme/api", orgB().ID, true)
		})
		v, out, err := teamsJSON(t, "--unbind", "github.com/acme/api")
		if err != nil {
			t.Fatalf("--unbind repo: %v\n%s", err, out)
		}
		joined := strings.Join(v.Changes, "\n")
		if !strings.Contains(joined, "under github.com/acme") || strings.Contains(joined, "free: personal may collect it") {
			t.Fatalf("changes = %v, want inherited owner status", v.Changes)
		}
	})

	t.Run("combined bind flags validate against evolving planned bindings", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		if _, _, err := teamsJSON(t, "--bind", "github.com/acme=Acme", "--bind", "github.com/acme/api=Beta"); err == nil {
			t.Fatal("expected conflict error without --yes on combined owner+repo binds")
		}
		if _, out, err := teamsJSON(t, "--bind", "github.com/acme=Acme", "--bind", "github.com/acme/api=Beta", "--yes"); err != nil {
			t.Fatalf("combined --bind --yes: %v\n%s", err, out)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if st.Bindings["github.com/acme"].Organization != orgA().ID || st.Bindings["github.com/acme/api"].Organization != orgB().ID {
			t.Fatalf("bindings = %+v", st.Bindings)
		}
	})

	t.Run("interactive whole-owner selection reconciles existing child repository bindings", func(t *testing.T) {
		for _, keepChild := range []bool{true, false} {
			name, answer := "unbind", "n\n"
			if keepChild {
				name, answer = "keep", "y\n"
			}
			t.Run(name, func(t *testing.T) {
				routedMachine(t, "alice", func(_ *fakeAuth, s *routing.State) error {
					return s.Bind("github.com/acme/other", orgB().ID, false)
				})
				prevAsk, prevPick := testApp.canAsk, testApp.pickOne
				t.Cleanup(func() { testApp.canAsk, testApp.pickOne = prevAsk, prevPick })
				testApp.canAsk = func() bool { return true }
				testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) { return 1, nil }
				cmd := testApp.NewRootCommand()
				var out strings.Builder
				cmd.SetIn(strings.NewReader(answer))
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs([]string{"teams", "--select", "Acme API"})
				if err := cmd.Execute(); err != nil {
					t.Fatalf("teams --select: %v\n%s", err, out.String())
				}
				st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
				if err != nil {
					t.Fatal(err)
				}
				if st.Bindings["github.com/acme"].Organization != orgA().ID {
					t.Fatalf("owner binding = %+v, want %s", st.Bindings["github.com/acme"], orgA().ID)
				}
				child, hasChild := st.Bindings["github.com/acme/other"]
				if keepChild {
					if !hasChild || child.Organization != orgB().ID {
						t.Fatalf("kept child = (%+v, %v), want %s", child, hasChild, orgB().ID)
					}
					if !strings.Contains(out.String(), "kept github.com/acme/other") {
						t.Fatalf("output missing kept warning:\n%s", out.String())
					}
				} else if hasChild {
					t.Fatalf("child binding = %+v, want unbound under whole owner", child)
				}
			})
		}
	})
}

func TestSetupInteractiveChecklistHookup(t *testing.T) {
	runSetup := func(t *testing.T, f *fakeAuth, account string, sf setupFlags) (*setupUI, error) {
		t.Helper()
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Account = account
		cfg.OrganizationID = orgA().ID
		cfg.OrganizationName = orgA().Name
		cfg.ProjectID = teamsAcmeWeb
		cmd := &cobra.Command{}
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetContext(t.Context())
		ui := newSetupUI(&out, false)
		org := orgA()
		_, _, err = testApp.setupTeams(cmd, cfg, &org, sf, ui)
		return ui, err
	}

	t.Run("interactive setup ticks second org team via checklist", func(t *testing.T) {
		f, account := routedMachine(t, "alice", nil)
		prevAsk, prevForm, prevPick := testApp.canAsk, testApp.runForm, testApp.pickOne
		t.Cleanup(func() { testApp.canAsk, testApp.runForm, testApp.pickOne = prevAsk, prevForm, prevPick })
		testApp.canAsk = func() bool { return true }
		testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) { return 0, nil }
		formCalled := false
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			formCalled = true
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Beta Core" {
					items[i].Selected = true
				}
			}
			return items, nil
		}
		if _, err := runSetup(t, f, account, setupFlags{}); err != nil {
			t.Fatalf("setupTeams: %v", err)
		}
		if !formCalled {
			t.Fatal("expected interactive setup to invoke multi-team checklist")
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, tm := range st.Teams {
			ids = append(ids, tm.TeamID)
		}
		if !slices.Contains(ids, teamsAcmeWeb) || !slices.Contains(ids, betaCore) {
			t.Fatalf("teams = %v, want both %s and %s", ids, teamsAcmeWeb, betaCore)
		}
		if st.Bindings["github.com/beta/core"].Organization != orgB().ID {
			t.Fatalf("beta binding = %+v, want %s", st.Bindings["github.com/beta/core"], orgB().ID)
		}
	})

	t.Run("stale generation during setup checklist fails closed without replaying consent", func(t *testing.T) {
		f, account := routedMachine(t, "alice", nil)
		prevAsk, prevForm, prevPick := testApp.canAsk, testApp.runForm, testApp.pickOne
		t.Cleanup(func() { testApp.canAsk, testApp.runForm, testApp.pickOne = prevAsk, prevForm, prevPick })
		testApp.canAsk = func() bool { return true }
		testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) { return 0, nil }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			// Concurrent write bumps Generation while the setup checklist is open.
			if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, routing.Expect{Account: account}, func(s *routing.State) error {
				return s.Refuse("github.com/concurrent/repo")
			}); err != nil {
				t.Fatalf("concurrent update: %v", err)
			}
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Beta Core" {
					items[i].Selected = true
				}
			}
			return items, nil
		}
		_, err := runSetup(t, f, account, setupFlags{})
		if !errors.Is(err, routing.ErrStaleGeneration) {
			t.Fatalf("err = %v, want ErrStaleGeneration", err)
		}
		st, _, err := routing.Load(testApp.stateDir, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(st.Teams, func(tm routing.Team) bool { return tm.TeamID == betaCore }) {
			t.Fatal("stale checklist consent must not tick Beta Core")
		}
	})

	t.Run("assumeYes skips interactive checklist", func(t *testing.T) {
		f, account := routedMachine(t, "alice", nil)
		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) {
			t.Fatal("runForm must not be called under --yes")
			return nil, nil
		}
		if _, err := runSetup(t, f, account, setupFlags{assumeYes: true}); err != nil {
			t.Fatalf("setupTeams -y: %v", err)
		}
	})

	t.Run("checklist cancellation propagates errCancelled to stop setup", func(t *testing.T) {
		f, account := routedMachine(t, "alice", nil)
		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) { return nil, prompt.ErrCancelled }
		_, err := runSetup(t, f, account, setupFlags{})
		if !errors.Is(err, errCancelled) {
			t.Fatalf("err = %v, want errCancelled", err)
		}
	})

	t.Run("deselecting initial Personal team returns final org policy and updates recorded team", func(t *testing.T) {
		f, account := routedMachine(t, "alice", nil)
		f.orgs = []organization{orgPersonal(), orgA(), orgB()}
		f.projects = map[string][]project{
			teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
			orgA().ID:        projectsIn(orgA().ID),
			orgB().ID:        projectsIn(orgB().ID),
		}
		prevAsk, prevForm, prevPick := testApp.canAsk, testApp.runForm, testApp.pickOne
		t.Cleanup(func() { testApp.canAsk, testApp.runForm, testApp.pickOne = prevAsk, prevForm, prevPick })
		testApp.canAsk = func() bool { return true }
		testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) { return 0, nil }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			items := slices.Clone(form.Items)
			for i := range items {
				items[i].Selected = items[i].Label == "Beta Core"
			}
			return items, nil
		}
		cfg, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Account = account
		cfg.OrganizationID = orgPersonal().ID
		cfg.OrganizationName = orgPersonal().Name
		cfg.ProjectID = teamsPersonalTeam
		cmd := &cobra.Command{}
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetContext(t.Context())
		ui := newSetupUI(&out, false)
		org := orgPersonal()
		pol, name, err := testApp.setupTeams(cmd, cfg, &org, setupFlags{}, ui)
		if err != nil {
			t.Fatalf("setupTeams: %v", err)
		}
		if pol.Global() || pol.TeamID != betaCore || name != "Beta Core" {
			t.Fatalf("returned (%+v, %q), want non-global Beta Core (%s)", pol, name, betaCore)
		}
		updated, err := testApp.loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if updated.Team != betaCore || updated.TeamName != "Beta Core" {
			t.Fatalf("recorded team = (%q, %q), want (%q, %q)", updated.Team, updated.TeamName, betaCore, "Beta Core")
		}
	})
}
