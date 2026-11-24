package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// teamsView is `terma teams`' list: every organization the developer belongs to,
// each organization's teams with what their policies collect, and the bindings.
type teamsView struct {
	Organizations []orgRow     `json:"organizations"`
	Unlisted      string       `json:"unlisted,omitempty"`
	Bindings      []bindingRow `json:"bindings"`
	Pending       []pendingRow `json:"pending"`
	Changes       []string     `json:"changes,omitempty"`
	Warnings      []string     `json:"warnings,omitempty"`
}

// orgRow is one organization and its teams, listed on the single user token.
type orgRow struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	Type         string    `json:"type,omitempty"`
	Inaccessible bool      `json:"inaccessible,omitempty"`
	Note         string    `json:"note,omitempty"`
	Teams        []teamRow `json:"teams"`
}

// teamRow is one team: whether it is ticked, and what its policy collects.
type teamRow struct {
	ID           string             `json:"id"`
	Name         string             `json:"name,omitempty"`
	Selected     bool               `json:"selected"`
	EnrollmentID string             `json:"enrollment_id,omitempty"`
	State        routing.TeamState  `json:"state,omitempty"`
	LastError    routing.ErrorClass `json:"last_error,omitempty"`
	Entries      []string           `json:"entries,omitempty"`
	Collects     string             `json:"collects"`
	GitHooks     bool               `json:"git_hooks,omitempty"`
	Unreadable   string             `json:"unreadable,omitempty"`
}

// bindingRow is one decision about an owner or a repository.
type bindingRow struct {
	Key          string `json:"key"`
	Organization string `json:"organization,omitempty"`
	Name         string `json:"organization_name,omitempty"`
	Refused      bool   `json:"refused,omitempty"`
}

// pendingRow is a binding left to decide, and the entry that needs it.
type pendingRow struct {
	Key          string `json:"key"`
	Organization string `json:"organization"`
	TeamID       string `json:"team_id"`
	Entry        string `json:"entry"`
}

const (
	listLimit           = 4
	personalCollects    = "everything not bound to an organization"
	orgInaccessibleNote = "you no longer have access to this organization"
)

func collectsSummary(pol config.Policy, personal bool) string {
	switch {
	case pol.Global() && personal:
		return personalCollects
	case pol.Global(), pol.Unset, len(pol.Repositories) == 0:
		return "nothing until an admin chooses repositories"
	}
	return strings.Join(pol.Repositories, ", ")
}

// listTeams lists every organization and its teams concurrently on one shared user-token
// client. 403 PROJECT_NOT_FOUND hides deleted teams; 403 ORG_NOT_ACCESSIBLE annotates the
// org without failing other orgs or Personal. Selected teams are always shown from st.
func (app *App) listTeams(ctx context.Context, cfg *config.Config, st routing.State) teamsView {
	client, err := app.teamsAPIClient(cfg, st.Account)
	return app.listTeamsWithClient(ctx, cfg, st, client, err)
}

func (app *App) listTeamsWithClient(ctx context.Context, cfg *config.Config, st routing.State, client *api.Client, clientErr error) teamsView {
	var v teamsView
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	var orgs []organization
	if clientErr != nil {
		v.Unlisted = clientErr.Error()
	} else if listed, err := fetchOrganizations(ctx, client); err != nil {
		v.Unlisted = err.Error()
	} else {
		orgs = listed
	}
	if len(orgs) > 0 {
		for i, listed := range app.fetchTeams(ctx, client, cfg.AuthURL, orgs) {
			o := orgs[i]
			row := orgRow{ID: o.ID, Name: o.Name, Type: o.Type}
			names.org(o.ID, o.Name)
			switch {
			case orgNotAccessibleErr(listed.err):
				row.Inaccessible, row.Note = true, orgInaccessibleNote
			case listed.err != nil:
				row.Note = "its teams could not be listed: " + listed.err.Error()
			}
			for _, p := range listed.teams {
				names.team(p.ID, p.Name)
				if teamNotFound(p.err) {
					continue
				}
				t := teamRow{ID: p.ID, Name: p.Name}
				switch {
				case orgNotAccessibleErr(p.err):
					row.Inaccessible, row.Note = true, orgInaccessibleNote
					t.Unreadable, t.Collects = orgInaccessibleNote, "its policy could not be read"
				case p.err != nil:
					t.Unreadable, t.Collects = p.err.Error(), "its policy could not be read"
				default:
					t.Entries, t.Collects, t.GitHooks = p.policy.Repositories, collectsSummary(p.policy, o.Type == "personal"), p.policy.GitHooks
				}
				row.Teams = append(row.Teams, t)
			}
			v.Organizations = append(v.Organizations, row)
		}
	}
	names.save(app.stateDir, cfg.ProfileName)
	v.mergeState(st, names, time.Now())
	return v
}

type listedOrg struct {
	err   error
	teams []listedTeam
}

type listedTeam struct {
	project
	policy config.Policy
	err    error
}

// fetchTeams lists each org's teams and reads each team's policy on one shared api.Client
// (serializing refreshes via tokenGen), capped at listLimit in-flight requests while
// preserving listing order.
func (app *App) fetchTeams(ctx context.Context, client *api.Client, authURL string, orgs []organization) []listedOrg {
	out := make([]listedOrg, len(orgs))
	slots := make(chan struct{}, listLimit)
	limited := func(request func()) bool {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			request()
			return true
		case <-ctx.Done():
			return false
		}
	}
	var wg sync.WaitGroup
	for i, o := range orgs {
		org := &out[i]
		wg.Go(func() {
			var projects []project
			if !limited(func() { projects, org.err = fetchProjects(ctx, client, o.ID) }) {
				org.err = ctx.Err()
				return
			}
			if org.err != nil {
				return
			}
			org.teams = make([]listedTeam, len(projects))
			for j, p := range projects {
				team := &org.teams[j]
				team.project = p
				wg.Go(func() {
					if !limited(func() { team.policy, team.err = fetchTeamPolicy(ctx, client, authURL, o.ID, p.ID) }) {
						team.err = ctx.Err()
					}
				})
			}
		})
	}
	wg.Wait()
	return out
}

// teamsAPIClient returns one shared API client authenticated with the profile's single
// user token, verified against account (State.Account).
func (app *App) teamsAPIClient(cfg *config.Config, account string) (*api.Client, error) {
	if cfg.APIKey != "" || cfg.ServerKeySignIn || account == sendas.NoPersonLane {
		return nil, errServerKeyTeams
	}
	cred, err := auth.LoadCredential(app.dir, cfg.ProfileName)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return nil, errors.New(app.notSignedIn())
	}
	if err != nil {
		return nil, err
	}
	if account != "" && cred.Account() != account {
		return nil, fmt.Errorf("%w: stored sign-in belongs to another account — run `terma setup`", routing.ErrAccountMismatch)
	}
	return api.New(cfg, api.Options{Version: app.version, Credential: cred})
}

func fetchTeamPolicy(ctx context.Context, client *api.Client, authURL, organizationID, teamID string) (config.Policy, error) {
	pol, err := client.TeamPolicy(ctx, teamID)
	if err != nil {
		return config.Policy{}, err
	}
	pol.OrganizationID, pol.AuthURL, pol.TeamID = organizationID, authURL, teamID
	if pol.Global() && config.PolicyStub() == "" {
		pol.DefaultProjectID = teamID
	}
	return pol, nil
}

// mergeState marks selected teams, adds any missing from the listing, and fills bindings
// and pending decisions.
func (v *teamsView) mergeState(st routing.State, names teamNames, now time.Time) {
	for i := range v.Organizations {
		for j := range v.Organizations[i].Teams {
			t := &v.Organizations[i].Teams[j]
			t.Selected, t.EnrollmentID, t.State, t.LastError = false, "", "", ""
		}
	}
	v.Bindings = nil
	for _, t := range st.Teams {
		org := v.org(t.OrganizationID, names)
		i := slices.IndexFunc(org.Teams, func(r teamRow) bool { return r.ID == t.TeamID })
		if i < 0 {
			org.Teams = append(org.Teams, teamRow{ID: t.TeamID, Name: names.Teams[t.TeamID]})
			i = len(org.Teams) - 1
		}
		row := &org.Teams[i]
		row.Selected, row.EnrollmentID, row.State, row.LastError = true, t.EnrollmentID, t.StateAt(now), t.LastError
		if row.Unreadable == "" && row.Entries == nil {
			row.Entries, row.Collects, row.GitHooks = t.Policy.Repositories, collectsSummary(t.Policy, t.Personal), t.Policy.GitHooks
		}
		if t.Personal && org.Type == "" {
			org.Type = "personal"
		}
	}
	for _, k := range slices.Sorted(maps.Keys(st.Bindings)) {
		b := st.Bindings[k]
		v.Bindings = append(v.Bindings, bindingRow{Key: k, Organization: b.Organization, Refused: b.Refused,
			Name: cmp.Or(names.Orgs[b.Organization], v.orgName(b.Organization))})
	}
	v.Pending = pendingBindings(st)
	// Personal is always last.
	slices.SortStableFunc(v.Organizations, func(a, b orgRow) int {
		return cmp.Compare(boolInt(a.Type == "personal"), boolInt(b.Type == "personal"))
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (v *teamsView) org(id string, names teamNames) *orgRow {
	if i := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == id }); i >= 0 {
		return &v.Organizations[i]
	}
	v.Organizations = append(v.Organizations, orgRow{ID: id, Name: names.Orgs[id], Note: "not listed for you now"})
	return &v.Organizations[len(v.Organizations)-1]
}

func (v *teamsView) orgName(id string) string {
	if i := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == id }); i >= 0 {
		return v.Organizations[i].Name
	}
	return ""
}

// pendingBindings lists the bindings ticked organizations' entries need with no decision yet.
func pendingBindings(st routing.State) []pendingRow {
	var out []pendingRow
	for _, t := range st.Teams {
		if t.Personal || t.Policy.Global() || t.Policy.Unset {
			continue
		}
		for _, raw := range t.Policy.Repositories {
			e, ok := routing.ParseEntry(raw, t.Policy.Wildcards)
			if !ok || slices.ContainsFunc(out, func(p pendingRow) bool { return p.Key == e.Key() }) {
				continue
			}
			_, hasKey := st.Bindings[e.Key()]
			_, hasOwner := st.Bindings[routing.Owner(e.Key())]
			if hasKey || (!e.Wildcard() && hasOwner) {
				continue
			}
			out = append(out, pendingRow{Key: e.Key(), Organization: t.OrganizationID, TeamID: t.TeamID, Entry: raw})
		}
	}
	return out
}

func teamNotFound(err error) bool {
	e, ok := errors.AsType[*api.Error](err)
	return ok && e.ProjectNotFound()
}
func orgNotAccessibleErr(err error) bool {
	e, ok := errors.AsType[*api.Error](err)
	return ok && e.OrgNotAccessible()
}
func teamLabel(names teamNames, t routing.Team) string {
	return orgTeamLabel(names.Orgs[t.OrganizationID], cmp.Or(names.Teams[t.TeamID], t.TeamID))
}

func orgTeamLabel(org, team string) string {
	if org == "" || strings.HasPrefix(team, org) {
		return team
	}
	return org + " " + team
}
