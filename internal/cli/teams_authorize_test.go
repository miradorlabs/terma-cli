package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// manyOrgs are n organizations besides Acme, in listing order.
func manyOrgs(n int) []organization {
	orgs := make([]organization, n)
	for i := range orgs {
		orgs[i] = organization{ID: fmt.Sprintf("44444444-0000-4000-8000-%012d", i+1), Name: fmt.Sprintf("Org %02d", i+1), Role: "member"}
	}
	return orgs
}

// The action names how many organizations one approval authorizes: every one up to the
// cap, else the first 20 of them in listing order.
func TestAuthorizeLabelAndBatch(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{1, "Authorize 1 more organization"},
		{3, "Authorize 3 more organizations"},
		{auth.MaxCodes, "Authorize 20 more organizations"},
		{25, "Authorize 20 of 25 more organizations"},
	} {
		if got := authorizeLabel(tc.n); got != tc.want {
			t.Errorf("authorizeLabel(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
	orgs := manyOrgs(25)
	if batch := authorizeBatch(orgs); len(batch) != auth.MaxCodes || batch[0] != orgs[0] || batch[19] != orgs[19] {
		t.Fatalf("batch = %d organizations from %s", len(batch), batch[0].Name)
	}
}

// The checklist offers the organizations without a sign-in as one action, after every
// team, never one line each.
func TestTeamsChecklistOffersOneAuthorizeAction(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	f.orgs = append([]organization{orgA()}, manyOrgs(24)...)
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	v := testApp.listTeams(t.Context(), cfg, loadState(t))
	if v.Unauthorized != 24 {
		t.Fatalf("unauthorized = %d, want 24", v.Unauthorized)
	}
	form, slots := teamsForm(v)
	var actions []prompt.Item
	for k, item := range form.Items {
		if slots[k].org < 0 {
			actions = append(actions, item)
		}
		if item.Label == "sign in to see its teams" {
			t.Errorf("an organization without a sign-in has its own line: %+v", item)
		}
	}
	if len(actions) != 1 || actions[0].Label != "Authorize 20 of 24 more organizations" || slots[len(slots)-1].org >= 0 {
		t.Fatalf("actions = %+v", actions)
	}
	if !strings.HasPrefix(actions[0].Detail, "Org 01, Org 02") || !strings.HasSuffix(actions[0].Detail, "Org 20") {
		t.Errorf("the action names %q, want the first 20 as listed", actions[0].Detail)
	}
	if teams := len(form.Items) - 1; teams != 2 {
		t.Errorf("%d team lines, want Acme's two", teams)
	}
}

// --json counts the organizations without a sign-in, 0 included, for the desktop.
func TestTeamsJSONCountsTheUnauthorizedOrganizations(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	threeOrgs(f)
	stdout, stderr, err := termaRun{}.exec(t, "teams", "--json")
	if err != nil {
		t.Fatalf("teams --json: %v\n%s", err, stderr)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil || raw["unauthorized"] != float64(2) {
		t.Fatalf("unauthorized = %v (%v):\n%s", raw["unauthorized"], err, stdout)
	}
	for _, o := range []organization{orgB(), clientCo} {
		if _, err := auth.SaveCredential(testApp.dir, config.DefaultProfile, storedSession(f, o)); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, err = termaRun{}.exec(t, "teams", "--json")
	if err != nil || !strings.Contains(stdout, `"unauthorized": 0`) {
		t.Fatalf("with every organization signed in (%v):\n%s", err, stdout)
	}
}

// --authorize signs in, in one approval, to every organization without a sign-in, pre-
// checking exactly those; the organization active before stays active, and --json prints
// the refreshed list.
func TestTeamsAuthorizeSignsInToTheOrganizationsWithoutOne(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	threeOrgs(f)
	storeActiveSignIn(t, f, orgA())
	b := newFakeBrowser(t, f)
	stdout, stderr, err := termaRun{stderr: b}.exec(t, "teams", "--authorize", "--json", "--no-browser")
	if err != nil {
		t.Fatalf("teams --authorize: %v\n%s", err, stderr)
	}
	links := b.approvalLinks()
	if len(links) != 1 || links[0].Get("orgs") != orgB().ID+","+clientCo.ID || links[0].Has("org") || links[0].Get("codes") != "multiple" {
		t.Fatalf("approval links = %v", links)
	}
	var v teamsView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("no JSON (%v):\n%s", err, stdout)
	}
	if v.Unauthorized != 0 || slices.ContainsFunc(v.Organizations, func(o orgRow) bool { return !o.SignedIn }) {
		t.Fatalf("after authorizing: %+v", v)
	}
	if _, _, ok := v.row(orgB().ID, betaCore); !ok {
		t.Errorf("Beta Labs' team is not listed after authorizing: %+v", v.Organizations)
	}
	if cred, profile := activeOrg(t); cred != orgA().ID || profile != orgA().ID {
		t.Fatalf("active = %s, profile %s: authorizing more moved it off Acme", cred, profile)
	}
	if !strings.Contains(stderr, "Signed in to Beta Labs and ClientCo\n") || strings.Contains(stderr, "Not signed in") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

// Over the cap, --authorize pre-checks the first 20 in listing order; what is not approved
// stays to authorize, and is named.
func TestTeamsAuthorizeTakesTwentyAtATime(t *testing.T) {
	f := migratedMachine(t, nil, orgA())
	others := manyOrgs(24)
	f.orgs = append([]organization{orgA()}, others...)
	storeActiveSignIn(t, f, orgA())
	b := ticking(t, f, others[0].ID)
	stdout, stderr, err := termaRun{stderr: b}.exec(t, "teams", "--authorize", "--json", "--no-browser")
	if err != nil {
		t.Fatalf("teams --authorize: %v\n%s", err, stderr)
	}
	var want []string
	for _, o := range others[:auth.MaxCodes] {
		want = append(want, o.ID)
	}
	if links := b.approvalLinks(); len(links) != 1 || links[0].Get("orgs") != strings.Join(want, ",") {
		t.Fatalf("approval links = %v, want the first 20 as listed", links)
	}
	var v teamsView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil || v.Unauthorized != 23 {
		t.Fatalf("unauthorized = %d (%v), want 23", v.Unauthorized, err)
	}
	if !strings.Contains(stderr, "Signed in to Org 01\n") || strings.Count(stderr, "Not signed in to ") != 1 || !strings.Contains(stderr, "Not signed in to 23 organizations, including ") {
		t.Fatalf("stderr:\n%s", stderr)
	}
}

// With every organization signed in, --authorize opens no browser and says so.
func TestTeamsAuthorizeWithEveryOrganizationSignedInIsANoOp(t *testing.T) {
	f := migratedMachine(t, nil, orgA(), orgB())
	b := newFakeBrowser(t, f)
	stdout, stderr, err := termaRun{stderr: b}.exec(t, "teams", "--authorize", "--json", "--no-browser")
	if err != nil {
		t.Fatalf("teams --authorize: %v\n%s", err, stderr)
	}
	if len(b.approvalLinks()) != 0 || f.exchanges.Load() != 0 {
		t.Fatalf("a no-op opened %d approvals", len(b.approvalLinks()))
	}
	if !strings.Contains(stderr, "Every organization you belong to already has a sign-in on this machine.") {
		t.Fatalf("stderr:\n%s", stderr)
	}
	var v teamsView
	if err := json.Unmarshal([]byte(stdout), &v); err != nil || v.Unauthorized != 0 || len(v.Organizations) != 2 {
		t.Fatalf("the list after a no-op (%v):\n%s", err, stdout)
	}
}
