package cli

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// teamsView is `terma teams`' list: every organization the developer belongs to, each
// signed-in one's teams with what their policies collect, and the bindings.
type teamsView struct {
	Organizations []orgRow `json:"organizations"`
	// Unlisted says why the organizations could not be listed; the selected teams are
	// still shown, from the routing state.
	Unlisted string       `json:"unlisted,omitempty"`
	Bindings []bindingRow `json:"bindings"`
	// Pending are the bindings a selected organization's entry needs and no one decided.
	Pending []pendingRow `json:"pending"`
	// Changes say what this run did, and Warnings what the developer should know of it.
	Changes  []string `json:"changes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// orgRow is one organization: its teams once this machine has its sign-in.
type orgRow struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	// SignedIn says this machine has the organization's sign-in, which listing its teams needs.
	SignedIn bool      `json:"signed_in"`
	Note     string    `json:"note,omitempty"`
	Teams    []teamRow `json:"teams"`
}

// teamRow is one team: whether it is ticked, and what its policy collects.
type teamRow struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Selected     bool   `json:"selected"`
	EnrollmentID string `json:"enrollment_id,omitempty"`
	// State and LastError are a selected team's, from the routing state.
	State     routing.TeamState  `json:"state,omitempty"`
	LastError routing.ErrorClass `json:"last_error,omitempty"`
	// Entries are its policy's repository list; Collects says it in words.
	Entries  []string `json:"entries,omitempty"`
	Collects string   `json:"collects"`
	// GitHooks says its policy stamps commits in what it collects.
	GitHooks bool `json:"git_hooks,omitempty"`
	// Unreadable says why its policy could not be read.
	Unreadable string `json:"unreadable,omitempty"`
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

// personalCollects is what a personal team collecting everything takes.
const personalCollects = "everything not bound to an organization"

// collectsSummary says what pol collects for a team, in the list's words.
func collectsSummary(pol config.Policy, personal bool) string {
	switch {
	case pol.Global() && personal:
		return personalCollects
	case pol.Global(), pol.Unset, len(pol.Repositories) == 0:
		return "nothing until an admin chooses repositories"
	}
	return strings.Join(pol.Repositories, ", ")
}

// listTeams lists every organization with one sign-in (contract C4), and each one this
// machine is signed in to with its teams and their entries, read with that organization's
// own sign-in. A team whose policy read says it is gone is hidden. The selected teams are
// always shown, from st, whatever the network says.
func (app *App) listTeams(ctx context.Context, cfg *config.Config, st routing.State) teamsView {
	var v teamsView
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	orgs, err := app.listOrganizations(ctx, cfg)
	if err != nil {
		v.Unlisted = err.Error()
	}
	for _, o := range orgs {
		row := orgRow{ID: o.ID, Name: o.Name, Type: o.Type}
		names.org(o.ID, o.Name)
		cred, ok := app.orgSignIn(cfg, o.ID)
		if !ok {
			row.Note = "sign in to see its teams"
			v.Organizations = append(v.Organizations, row)
			continue
		}
		row.SignedIn = true
		projects, err := app.orgProjects(ctx, cfg, cred)
		if err != nil {
			row.Note = "its teams could not be listed: " + err.Error()
		}
		for _, p := range projects {
			names.team(p.ID, p.Name)
			pol, err := app.teamPolicy(ctx, cfg, cred, o.ID, p.ID)
			if teamNotFound(err) {
				continue
			}
			t := teamRow{ID: p.ID, Name: p.Name}
			if err != nil {
				t.Unreadable, t.Collects = err.Error(), "its policy could not be read"
			} else {
				t.Entries, t.Collects, t.GitHooks = pol.Repositories, collectsSummary(pol, o.Type == "personal"), pol.GitHooks
			}
			row.Teams = append(row.Teams, t)
		}
		v.Organizations = append(v.Organizations, row)
	}
	names.save(app.stateDir, cfg.ProfileName)
	v.mergeState(st, names, time.Now())
	return v
}

// mergeState marks the selected teams, adding any the listing lacks, and fills in the
// bindings and what is pending.
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
	keys := make([]string, 0, len(st.Bindings))
	for k := range st.Bindings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		b := st.Bindings[k]
		v.Bindings = append(v.Bindings, bindingRow{Key: k, Organization: b.Organization, Refused: b.Refused,
			Name: cmp.Or(names.Orgs[b.Organization], v.orgName(b.Organization))})
	}
	v.Pending = pendingBindings(st)
	// Personal is always last.
	slices.SortStableFunc(v.Organizations, func(a, b orgRow) int {
		return boolOrder(a.Type == "personal", b.Type == "personal")
	})
}

func boolOrder(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// org is the row for organization id, added from names when the listing lacked it.
func (v *teamsView) org(id string, names teamNames) *orgRow {
	for i := range v.Organizations {
		if v.Organizations[i].ID == id {
			return &v.Organizations[i]
		}
	}
	v.Organizations = append(v.Organizations, orgRow{ID: id, Name: names.Orgs[id], Note: "not listed for you now"})
	return &v.Organizations[len(v.Organizations)-1]
}

// orgName is organization id's name as listed, else "".
func (v *teamsView) orgName(id string) string {
	for _, o := range v.Organizations {
		if o.ID == id {
			return o.Name
		}
	}
	return ""
}

// pendingBindings are the bindings a ticked organization's entry needs that no decision
// covers, each once: Route holds what such an entry matches until the developer decides.
func pendingBindings(st routing.State) []pendingRow {
	var out []pendingRow
	for _, p := range doctor.Pending(st) {
		out = append(out, pendingRow{Key: p.Key, Organization: p.Organization, TeamID: p.TeamID, Entry: p.Entry})
	}
	return out
}

// listOrganizations lists the developer's organizations with any one working sign-in.
func (app *App) listOrganizations(ctx context.Context, cfg *config.Config) ([]organization, error) {
	if cfg.APIKey != "" {
		return nil, errors.New("TERMA_API_KEY is set: a server key lists no organizations")
	}
	client, err := app.workingClient(ctx, cfg)
	if errors.Is(err, errNoWorkingCredential) {
		return nil, errors.New("not signed in: `terma setup` signs you in")
	}
	if err != nil {
		return nil, err
	}
	return fetchOrganizations(ctx, client)
}

// orgSignIn is this machine's sign-in for organizationID in cfg's environment.
func (app *App) orgSignIn(cfg *config.Config, organizationID string) (*auth.Credential, bool) {
	cred, err := auth.LoadCredentialFor(app.dir, cfg.ProfileName, organizationID)
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil {
		return nil, false
	}
	return cred, true
}

// orgProjects lists an organization's teams with its own sign-in.
func (app *App) orgProjects(ctx context.Context, cfg *config.Config, cred *auth.Credential) ([]project, error) {
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return nil, err
	}
	return fetchProjects(ctx, client)
}

// teamPolicy fetches team's policy with its organization's sign-in, stamped with the
// organization, environment and team it was fetched for, as the refresher stamps them.
func (app *App) teamPolicy(ctx context.Context, cfg *config.Config, cred *auth.Credential, organizationID, team string) (config.Policy, error) {
	scoped := *cfg
	scoped.APIKey, scoped.ProjectID = "", team
	client, err := api.New(&scoped, api.Options{Version: app.version, ProjectID: team, Credential: cred})
	if err != nil {
		return config.Policy{}, err
	}
	pol, err := client.CollectionPolicy(ctx)
	if err != nil {
		return config.Policy{}, err
	}
	pol.OrganizationID, pol.AuthURL, pol.TeamID = organizationID, cfg.AuthURL, team
	return pol, nil
}

// teamNotFound reports a policy read refused because the team is not an active one of the
// organization (contract C3: 403 PERMISSION_DENIED, reason TEAM_NOT_FOUND). The account
// service gives that one answer for a missing, archived, deleted or other organization's
// team, so a 403 to the organization's own sign-in says as much as the reason does.
func teamNotFound(err error) bool {
	apiErr, ok := errors.AsType[*api.Error](err)
	return ok && apiErr.StatusCode == http.StatusForbidden
}

// teamLabel names a team for a message: its name and organization's, else their ids.
func teamLabel(names teamNames, t routing.Team) string {
	return orgTeamLabel(names.Orgs[t.OrganizationID], cmp.Or(names.Teams[t.TeamID], t.TeamID))
}

// orgTeamLabel names a team with its organization, once when the team's name already
// begins with it ("Acme API", not "Acme Acme API").
func orgTeamLabel(org, team string) string {
	if org == "" || strings.HasPrefix(team, org) {
		return team
	}
	return org + " " + team
}
