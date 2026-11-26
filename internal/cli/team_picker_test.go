package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

// Setup's team picker shows a team's description, never its policy, and the personal
// organization first, as one "Personal" item with no heading.
func TestSetupTeamPickerDescriptionsAndPersonal(t *testing.T) {
	gateway := newFakeAuth(t)
	gateway.orgs = []organization{orgA(), orgPersonal()}
	gateway.projects = map[string][]project{
		orgA().ID: {
			{ID: teamsAcmeWeb, Name: "Acme Web", Description: "The web\n  front end", OrganizationID: orgA().ID},
			{ID: acmeAPI, Name: "Acme API", OrganizationID: orgA().ID},
		},
		teamsPersonalOrg: {{ID: teamsPersonalTeam, Name: "Default", OrganizationID: teamsPersonalOrg}},
	}
	gateway.policyBodies = map[string]string{
		teamsAcmeWeb:      policyBody("github.com/acme/web"),
		acmeAPI:           policyBody("github.com/acme/api"),
		teamsPersonalTeam: globalBody,
	}
	setupMachine(t, gateway)

	prevAsk, prevForm := testApp.canAsk, testApp.runForm
	t.Cleanup(func() { testApp.canAsk, testApp.runForm = prevAsk, prevForm })
	testApp.canAsk = func() bool { return true }
	var teams []prompt.Item
	testApp.runForm = func(form *prompt.Form) ([]prompt.Item, error) {
		if form.Title == teamPickerTitle {
			teams = form.Items
		}
		return form.Items, nil
	}
	if out, err := runTerma(t, "setup"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	want := []prompt.Item{
		{Label: "Personal", Kind: prompt.Check, Selected: true},
		{Label: "Acme Web", Detail: "The web front end", Kind: prompt.Check, Selected: true, Heading: orgA().Name},
		{Label: "Acme API", Kind: prompt.Check, Selected: true},
	}
	if len(teams) != len(want) {
		t.Fatalf("team picker items = %+v, want %+v", teams, want)
	}
	for i := range want {
		if teams[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, teams[i], want[i])
		}
	}
}

// `terma teams`' picker shows descriptions and state notes, not policies, and the personal
// organization's one team first, as "Personal".
func TestTeamsFormDescriptionsAndPersonal(t *testing.T) {
	v := teamsView{Organizations: []orgRow{
		{ID: "o1", Name: "Acme", Teams: []teamRow{
			{ID: "t1", Name: "Acme Web", Description: "The web front end", Collects: "github.com/acme/web", Selected: true},
			{ID: "t2", Name: "Acme API", Collects: "github.com/acme/api", Selected: true, State: routing.Suspended},
			{ID: "t3", Name: "Acme Docs", Collects: "github.com/acme/docs"},
		}},
		{ID: "p1", Name: "Someone's Personal", Type: "personal", Teams: []teamRow{{ID: "pt", Name: "Default", Collects: personalCollects, Selected: true}}},
	}}
	form, slots := teamsForm(v)
	if len(form.Items) != 4 {
		t.Fatalf("items = %+v", form.Items)
	}
	if got := form.Items[0]; got.Label != "Personal" || got.Heading != "" || got.Detail != "" || slots[0] != (checklistSlot{1, 0}) {
		t.Errorf("personal item = %+v at slot %+v, want it first", got, slots[0])
	}
	form.Items, slots = form.Items[1:], slots[1:]
	if got := form.Items[0]; got.Detail != "The web front end" || got.Heading != "Acme" {
		t.Errorf("described team = %+v", got)
	}
	if got := form.Items[1].Detail; !strings.HasPrefix(got, "— suspended") || strings.Contains(got, "acme/api") {
		t.Errorf("suspended team detail = %q, want its state note alone", got)
	}
	if got := form.Items[2].Detail; got != "" {
		t.Errorf("undescribed team detail = %q, want none", got)
	}
	if slots[0] != (checklistSlot{0, 0}) {
		t.Errorf("first organization team at slot %+v", slots[0])
	}

	v.state = routing.State{Teams: []routing.Team{{TeamID: "t1", OrganizationID: "o1"}, {TeamID: "pt", OrganizationID: "p1", Personal: true}}}
	var out bytes.Buffer
	renderTeams(&out, v)
	if text := flat(out.String()); !strings.Contains(text, "✓ Teams Acme Web, Personal") || strings.Contains(text, "Default") || strings.Contains(text, "conflict") {
		t.Errorf("teams text is not the short summary:\n%s", out.String())
	}
}
