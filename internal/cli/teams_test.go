package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

	// Text output is a short summary, with no --authorize prompt.
	f.forbiddenTeams = nil
	text, err := runTerma(t, "teams")
	if err != nil {
		t.Fatalf("teams text: %v\n%s", err, text)
	}
	if !strings.Contains(flat(text), "! Teams None selected.") || strings.Contains(text, "[ ]") || strings.Contains(text, personalCollects) {
		t.Errorf("text output is not the short summary:\n%s", text)
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
			// Text rendering is the short summary, with no organization listing.
			text, err := runTerma(t, "teams")
			if err != nil {
				t.Fatalf("teams text: %v\n%s", err, text)
			}
			if !strings.Contains(text, "Teams") || strings.Contains(text, "Beta Labs") {
				t.Errorf("unexpected text output:\n%s", text)
			}
			// Attempting to tick a team in the inaccessible org fails cleanly.
			if _, _, err := teamsJSON(t, "--select", betaCore); err == nil {
				t.Error("selecting a team in an ORG_NOT_ACCESSIBLE org should fail")
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

	stdout, stderr, err := termaRun{}.exec(t, "teams")
	if err != nil {
		t.Fatalf("teams --yes: %v\n%s\n%s", err, stdout, stderr)
	}
	st := loadState(t)
	if len(st.Teams) != 1 || !slices.Equal(st.Teams[0].Policy.Repositories, []string{"github.com/acme/payments"}) {
		t.Fatalf("ticked a stale policy: %+v\n%s", st.Teams, stdout)
	}
}

// Invariant 2 (Council 1): an open interactive `terma teams` session captures
// (Account, StateID, ConsentGeneration) before prompting. A concurrent consent edit or a
// cross-account switch (Alice -> Bob) rejects Alice's confirm without replaying stale
// consent, whereas a background policy refresh (Generation only) does not cause a
// spurious conflict.
func TestTeamsCASRejectsStaleConsentAndAccountSwitch(t *testing.T) {
	t.Run("stale consent generation rejects Alice's open session", func(t *testing.T) {
		f, aliceAccount := routedMachine(t, "alice", nil)
		prevAsk, prevForm := testApp.canAsk, testApp.runForm
		t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
		testApp.canAsk = func() bool { return true }
		testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
			// While Alice's checklist is open, another `terma teams` command ticks a team
			// (advancing ConsentGeneration).
			cur := loadState(t)
			if _, err := routing.Update(testApp.stateDir, config.DefaultProfile, cur.ExpectConsent(), func(s *routing.State) error {
				_, err := s.Select(stateTeam(f, orgB().ID, betaCore, false, "github.com/beta/core"))
				return err
			}); err != nil {
				t.Fatalf("concurrent tick: %v", err)
			}
			items := slices.Clone(form.Items)
			for i := range items {
				if items[i].Label == "Acme API" {
					items[i].Selected = true
				}
			}
			return items, nil
		}

		_, _, err := termaRun{}.exec(t, "teams")
		if !errors.Is(err, routing.ErrStaleGeneration) {
			t.Fatalf("got err = %v, want routing.ErrStaleGeneration", err)
		}
		st := loadState(t)
		if st.Account != aliceAccount || len(st.Teams) != 1 || st.Teams[0].TeamID != betaCore {
			t.Fatalf("stale consent was replayed into state: %+v", st)
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

		_, _, err := termaRun{}.exec(t, "teams")
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

		stdout, stderr, err := termaRun{}.exec(t, "teams")
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
		if _, _, err := teamsJSON(t, "--select", acmeAPI); !errors.Is(err, routing.ErrAccountMismatch) {
			t.Fatalf("got err = %v, want routing.ErrAccountMismatch", err)
		}
		if st := loadState(t); len(st.Teams) != 0 {
			t.Fatalf("team ticked despite account mismatch: %+v", st.Teams)
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
// policy file, handles 2.x wildcard policies, reports overlapping policies, and downgrades
// a shared org's global policy.
func TestTeamsSelectWildcardOverlapGlobalAndNoKeyMinting(t *testing.T) {
	t.Run("select wildcard mints no key", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", nil)
		f.policyBodies[acmeAPI] = wildcardPolicyBody("github.com/acme/*")
		_, out, err := teamsJSON(t, "--select", "Acme API")
		if err != nil {
			t.Fatalf("teams --select: %v\n%s", err, out)
		}
		st := loadState(t)
		if len(st.Teams) != 1 || !st.Teams[0].Policy.Wildcards || !slices.Equal(st.Teams[0].Policy.Repositories, []string{"github.com/acme/*"}) {
			t.Fatalf("ticked wildcard team = %+v", st.Teams)
		}
		if key, err := keystore.Get(testApp.dir, acmeAPI); err != nil || key != "" {
			t.Errorf("keystore has minted key %q (err=%v), want none", key, err)
		}
		if pol, ok, err := config.ReadPolicy(testApp.stateDir, acmeAPI); err != nil || !ok || !pol.Wildcards {
			t.Errorf("stored policy file = (%+v, %v, %v)", pol, ok, err)
		}
	})

	t.Run("an overlap with a newer policy is reported", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			tm := stateTeam(f, orgB().ID, betaCore, false, "github.com/acme/api")
			tm.Policy.UpdatedAt = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
			_, err := s.Select(tm)
			return err
		})
		f.policyBodies[acmeAPI] = wildcardPolicyBody("github.com/acme/*")
		v, out, err := teamsJSON(t, "--select", "Acme API")
		if err != nil {
			t.Fatalf("teams --select: %v\n%s", err, out)
		}
		if len(v.Overlaps) != 1 || v.Overlaps[0].Entry != "github.com/acme/api" || !strings.Contains(v.Overlaps[0].Team, "Beta") {
			t.Fatalf("overlaps = %+v, want github.com/acme/api to Beta Core", v.Overlaps)
		}
		if len(v.Order) != 2 || !strings.Contains(v.Order[0], "Beta") || !strings.Contains(v.Order[1], "Acme API") {
			t.Fatalf("order = %v, want Beta Core then Acme API", v.Order)
		}
		text, err := runTerma(t, "teams")
		if err != nil || !strings.Contains(flat(text), "! Conflict Beta Core and Acme API list the same repository.") || !strings.Contains(text, conflictStep) || strings.Contains(text, "Routing order") {
			t.Fatalf("teams text lacks the one conflict line (%v):\n%s", err, text)
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

// Deselect unticks a team, and --release removes only Suspended or Refused teams.
func TestTeamsDeselectAndReleaseRequiresBlockedState(t *testing.T) {
	t.Run("deselect leaves personal", func(t *testing.T) {
		f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
			if _, err := s.Select(stateTeam(f, orgA().ID, acmeAPI, false, "github.com/acme/*")); err != nil {
				return err
			}
			_, err := s.Select(stateTeam(f, teamsPersonalOrg, teamsPersonalTeam, true))
			return err
		})
		f.orgs = []organization{orgA(), orgPersonal()}
		f.projects = map[string][]project{
			orgA().ID:        projectsIn(orgA().ID),
			teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Personal", OrganizationID: teamsPersonalOrg}},
		}

		_, out, err := teamsJSON(t, "--deselect", acmeAPI)
		if err != nil {
			t.Fatalf("teams --deselect: %v\n%s", err, out)
		}
		if st := loadState(t); len(st.Teams) != 1 || !st.Teams[0].Personal {
			t.Fatalf("state after deselect = %+v", st)
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
		if !strings.Contains(flat(text), "! Signed out Acme API. Run `terma setup` to sign in again.") || strings.Contains(text, "--org") {
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

		if _, _, err := teamsJSON(t, "--select", "Platform"); err == nil || !strings.Contains(err.Error(), "matches several") {
			t.Fatalf("duplicate --select err = %v, want ambiguity error", err)
		}
		if _, out, err := teamsJSON(t, "--select", dupB.ID); err != nil {
			t.Fatalf("--select by id: %v\n%s", err, out)
		}
		st, _, _ := routing.Load(testApp.stateDir, config.DefaultProfile)
		if len(st.Teams) != 1 || st.Teams[0].TeamID != dupB.ID {
			t.Fatalf("selected teams = %+v, want only %s", st.Teams, dupB.ID)
		}
	})

	t.Run("deselect preserves shared team policy file", func(t *testing.T) {
		routedMachine(t, "alice", nil)
		if _, out, err := teamsJSON(t, "--select", "Acme API"); err != nil {
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

}

// With --yes, setup ticks every team without asking.
func TestSetupYesEnrollsEveryTeamWithoutAsking(t *testing.T) {
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
	testApp.runForm = func(*prompt.Form) ([]prompt.Item, error) {
		t.Fatal("setup --yes must not open a picker")
		return nil, nil
	}
	testApp.pickOne = func(*cobra.Command, string, []pickRow, func(string) (int, error)) (int, error) {
		t.Fatal("setup must not open a team or org picker")
		return 0, nil
	}

	out, err := runTerma(t, "setup", "--harness", "codex", "--yes")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile)
	if err != nil || !found {
		t.Fatalf("routing.Load = %v, found=%v", err, found)
	}
	if st.Account != account {
		t.Fatalf("account = %q, want %q", st.Account, account)
	}
	var ids []string
	for _, tm := range st.Teams {
		ids = append(ids, tm.TeamID)
	}
	for _, want := range []string{teamsAcmeWeb, betaCore, teamsPersonalTeam} {
		if !slices.Contains(ids, want) {
			t.Fatalf("teams = %v, missing %s", ids, want)
		}
	}
	if !st.Teams[len(st.Teams)-1].Personal {
		t.Fatalf("last team = %+v, want Personal last", st.Teams[len(st.Teams)-1])
	}
}

func TestTeamsSelectFailsWhenPolicyCannotBePersisted(t *testing.T) {
	routedMachine(t, "u-1", nil)
	// Replace the policies directory with a regular file so StoreTeamPolicy fails deterministically.
	polDir := filepath.Join(testApp.stateDir, config.PoliciesDir)
	if err := os.RemoveAll(polDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(polDir, []byte("not-a-dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runTerma(t, "teams", "--select", betaCore)
	if err == nil {
		t.Fatalf("expected error when policy persistence fails, got output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "save policy for") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "ticked ") {
		t.Fatalf("must not report successful tick when policy persistence fails:\n%s", out)
	}
}
