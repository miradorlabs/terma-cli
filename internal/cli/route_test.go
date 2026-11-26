package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

const routeOwnOrg = "33333333-3333-4333-8333-333333333333"

func routePlanState(t *testing.T) (routing.State, teamNames) {
	t.Helper()
	const authURL = "https://auth.example.test"
	team := func(org, id string, personal bool, repos ...string) routing.Team {
		pol := config.Policy{
			Mode: config.ModeRepo, Repositories: repos, Revision: 1, FetchedAt: time.Now().UTC(),
			OrganizationID: org, AuthURL: authURL, TeamID: id, Wildcards: true,
		}
		if personal {
			pol.Mode = config.ModeGlobal
		}
		return routing.Team{OrganizationID: org, AuthURL: authURL, TeamID: id, Personal: personal, Policy: pol}
	}
	st := routing.State{Profile: config.DefaultProfile, Account: "0123456789abcdef"}
	for _, tm := range []routing.Team{
		team(orgA().ID, teamsAcmeWeb, false, "github.com/acme/*"),
		team(orgB().ID, betaCore, false, "github.com/clientco/app"),
		team(routeOwnOrg, teamsPersonalTeam, true),
	} {
		if _, err := st.Select(tm); err != nil {
			t.Fatal(err)
		}
	}
	names := teamNames{
		Teams: map[string]string{teamsAcmeWeb: "Platform", betaCore: "Contractors", teamsPersonalTeam: "Personal"},
		Orgs:  map[string]string{orgA().ID: "Acme", orgB().ID: "Beta Labs", routeOwnOrg: "Personal"},
	}
	return st, names
}

func explainText(st routing.State, names teamNames, repo string) (string, routeExplainView) {
	loc := routing.Location{Kind: routing.OutsideGit}
	if repo != "" {
		loc = routing.Location{Kind: routing.Repository, Repository: repo}
	}
	v := buildRouteExplain("", st, names, loc, time.Now())
	var b bytes.Buffer
	renderRouteExplain(&b, v)
	return b.String(), v
}

func wantRouteLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("route explain lacks %q:\n%s", w, out)
		}
	}
}

// route explain matches Router.Route across exact, wildcard, personal, an overlap the
// newer policy wins, blocked team states (suspended, expired, sign_in_needed), and a refused
// team passed over.
func TestRouteExplainMatchesRouterAcrossAllCases(t *testing.T) {
	st, names := routePlanState(t)

	// Wildcard, exact, personal, outside git.
	out, v := explainText(st, names, "git@github.com:Acme/api.git")
	wantRouteLines(t, out, "Goes to:   Acme Platform", "Entry:     github.com/acme/*")
	if v.Outcome != routing.Send || v.Team == nil || v.Team.TeamID != teamsAcmeWeb {
		t.Fatalf("wildcard view = %+v", v)
	}
	out, _ = explainText(st, names, "https://github.com/clientco/app")
	wantRouteLines(t, out, "Goes to:   Beta Labs Contractors", "Entry:     github.com/clientco/app")
	out, _ = explainText(st, names, "github.com/dawson/side")
	wantRouteLines(t, out, "Goes to:   Personal\n")
	out, _ = explainText(st, names, "")
	wantRouteLines(t, out, "Location:  outside any repository", "Goes to:   Personal\n")

	// Two organizations list it: the newer policy collects, and the other is named.
	overlapSt, _ := routePlanState(t)
	overlapSt.Teams[1].Policy.Repositories = []string{"github.com/acme/api"}
	overlapSt.Teams[0].Policy.UpdatedAt = time.Now().Add(-48 * time.Hour)
	overlapSt.Teams[1].Policy.UpdatedAt = time.Now().Add(-time.Hour)
	out, v = explainText(overlapSt, names, "github.com/acme/api")
	wantRouteLines(t, out, "Goes to:   Beta Labs Contractors", "Acme Platform lists github.com/acme/*, but a more recently updated policy collects it")
	if v.Outcome != routing.Send || v.Team == nil || v.Team.TeamID != betaCore {
		t.Fatalf("overlap view = %+v", v)
	}

	// Blocked states: SignInNeeded (single terma setup fix), Suspended, Expired; a refused team is passed over.
	signInSt, _ := routePlanState(t)
	signInSt.Teams[0].SignInNeeded = true
	out, v = explainText(signInSt, names, "github.com/acme/api")
	wantRouteLines(t, out, "needs you to sign in again", "Fix:       terma setup")
	if v.Outcome != routing.Hold || v.Blocked != routing.SignInNeeded || v.Fix != "terma setup" {
		t.Fatalf("sign_in_needed view = %+v", v)
	}

	orgLostSt, _ := routePlanState(t)
	orgLostSt.Teams[0].Refusals = 2
	orgLostSt.Teams[0].LastError = routing.OrgAccessLost
	out, v = explainText(orgLostSt, names, "github.com/acme/api")
	wantRouteLines(t, out, "Goes to:   Personal", "Acme Platform lists github.com/acme/*, but it is refused ("+orgInaccessibleNote+"), so it is passed over")
	if v.Outcome != routing.Send || v.Team == nil || !v.Team.Personal {
		t.Fatalf("org_access_lost view = %+v", v)
	}

	suspSt, _ := routePlanState(t)
	suspSt.Teams[0].Refusals = 1
	suspSt.Teams[0].LastError = routing.Terminal
	out, v = explainText(suspSt, names, "github.com/acme/api")
	wantRouteLines(t, out, "is suspended (its organization says the team is gone)")
	if v.Blocked != routing.Suspended {
		t.Fatalf("suspended view = %+v", v)
	}

	expSt, _ := routePlanState(t)
	expSt.Teams[0].Policy.FetchedAt = time.Now().Add(-config.MaxPolicyAge - time.Hour)
	out, v = explainText(expSt, names, "github.com/acme/api")
	wantRouteLines(t, out, "is expired")
	if v.Blocked != routing.Expired {
		t.Fatalf("expired view = %+v", v)
	}
}

func TestRouteExplainUnknownRule(t *testing.T) {
	st, names := routePlanState(t)
	out, _ := explainText(st, names, "")
	wantRouteLines(t, out, "Unknown:   a record whose location is unknown is held, then dropped: personal is not the only ticked team")
	st.Deselect(st.Teams[0].EnrollmentID)
	st.Deselect(st.Teams[0].EnrollmentID)
	out, v := explainText(st, names, "")
	wantRouteLines(t, out, "Unknown:   a record whose location is unknown goes to personal")
	if !v.UnknownToPersonal {
		t.Fatalf("expected UnknownToPersonal=true, got %+v", v)
	}
}

// End-to-end CLI tests for terma route explain: cwd, explicit path, --json, -o json,
// missing/unreadable/wrong-account routing.State, and server-key profile.
func TestRouteExplainCLIModesAndFailClosedStates(t *testing.T) {
	repo := gitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/acme/web.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}

	f, _ := routedMachine(t, "alice", func(f *fakeAuth, s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
		return err
	})

	// Explicit path + human output.
	out, err := runTerma(t, "route", "explain", repo)
	if err != nil {
		t.Fatalf("route explain: %v\n%s", err, out)
	}
	wantRouteLines(t, out, "Location:  github.com/acme/web (a repository)", "Goes to:   "+teamsAcmeWeb, "Entry:     github.com/acme/web")

	// Cwd + --json and -o json.
	t.Chdir(repo)
	for _, flags := range [][]string{{"route", "explain", "--json"}, {"route", "explain", "-o", "json"}} {
		stdout, stderr, err := termaRun{}.exec(t, flags...)
		if err != nil {
			t.Fatalf("%v: %v\n%s\n%s", flags, err, stdout, stderr)
		}
		var v routeExplainView
		if err := json.Unmarshal([]byte(stdout), &v); err != nil {
			t.Fatalf("%v non-JSON (%v):\n%s", flags, err, stdout)
		}
		if v.Outcome != routing.Send || v.Reason != string(routing.ReasonMatched) || v.Team == nil || v.Team.TeamID != teamsAcmeWeb {
			t.Fatalf("%v view = %+v", flags, v)
		}
	}

	// Unsupported output format fails.
	if _, _, err := (termaRun{}).exec(t, "route", "explain", "-o", "yaml"); err == nil {
		t.Fatal("expected error for -o yaml")
	}

	// Wrong-account routing.State fails closed.
	otherAcct := auth.AccountKey(f.srv.URL, "bob")
	if _, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, otherAcct, func(s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := termaRun{}.exec(t, "route", "explain", "--json")
	if err != nil {
		t.Fatalf("wrong-account route explain --json: %v", err)
	}
	var wrongV routeExplainView
	if err := json.Unmarshal([]byte(stdout), &wrongV); err != nil || wrongV.StateReason != "wrong_account" || wrongV.Outcome != routing.Drop || wrongV.Fix != "terma setup" {
		t.Fatalf("wrong-account view = %+v (err=%v)", wrongV, err)
	}

	// Corrupt/unreadable routing.State fails closed.
	p, err := routing.StatePath(testApp.stateDir, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = runTerma(t, "route", "explain")
	if err != nil {
		t.Fatalf("unreadable route explain: %v\n%s", err, out)
	}
	wantRouteLines(t, out, "nothing leaves this machine", "cannot be read", "Fix:       terma setup")

	// Missing routing.State fails closed.
	_ = os.Remove(p)
	stdout, _, err = termaRun{}.exec(t, "route", "explain", "--json")
	if err != nil {
		t.Fatalf("missing route explain --json: %v", err)
	}
	var missingV routeExplainView
	if err := json.Unmarshal([]byte(stdout), &missingV); err != nil || missingV.StateReason != "missing" || missingV.Outcome != routing.Drop {
		t.Fatalf("missing view = %+v (err=%v)", missingV, err)
	}

	// Server-key profile (no_person lane) routes cleanly and reports server_key: true.
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(pr *config.Profile) {
		pr.ServerKeySignIn = true
		pr.ServerKeyAuthURL = f.srv.URL
		pr.Team, pr.TeamName = teamsAcmeWeb, "Acme Web"
		pr.OrganizationID, pr.OrganizationName = orgA().ID, "Acme"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, sendas.NoPersonLane, func(s *routing.State) error {
		_, err := s.Select(stateTeam(f, orgA().ID, teamsAcmeWeb, false, "github.com/acme/web"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out, err = runTerma(t, "route", "explain")
	if err != nil {
		t.Fatalf("server-key route explain: %v\n%s", err, out)
	}
	wantRouteLines(t, out, "Goes to:   Acme Web", "Profile:   server key (one team)")
	stdout, _, err = termaRun{}.exec(t, "route", "explain", "--json")
	if err != nil {
		t.Fatalf("server-key route explain --json: %v", err)
	}
	var skV routeExplainView
	if err := json.Unmarshal([]byte(stdout), &skV); err != nil || !skV.ServerKey || skV.Outcome != routing.Send || skV.Team == nil || skV.Team.TeamID != teamsAcmeWeb {
		t.Fatalf("server-key view = %+v (err=%v)", skV, err)
	}

	// Malformed .git marker must fail closed with an error instead of falling back to OutsideGit/Personal.
	brokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenDir, ".git"), []byte("not-a-gitdir-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, sendas.NoPersonLane, func(s *routing.State) error {
		tm := stateTeam(f, teamsPersonalOrg, teamsPersonalTeam, true)
		tm.Policy.Mode = config.ModeGlobal
		_, err := s.Select(tm)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := runTerma(t, "route", "explain", brokenDir); err == nil {
		t.Fatalf("expected error on broken .git marker, got:\n%s", out)
	}
}
