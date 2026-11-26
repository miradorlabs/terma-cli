package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

type teamsView struct {
	Organizations []orgRow     `json:"organizations"`
	Unlisted      string       `json:"unlisted,omitempty"`
	Overlaps      []overlapRow `json:"overlaps"`
	// Order is the order routing tries the ticked teams in, while any policies overlap.
	Order    []string `json:"order,omitempty"`
	Changes  []string `json:"changes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	state    routing.State
}

type orgRow struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	Type         string    `json:"type,omitempty"`
	Inaccessible bool      `json:"inaccessible,omitempty"`
	Note         string    `json:"note,omitempty"`
	Teams        []teamRow `json:"teams"`
}

type teamRow struct {
	ID           string             `json:"id"`
	Name         string             `json:"name,omitempty"`
	Description  string             `json:"description,omitempty"`
	Selected     bool               `json:"selected"`
	EnrollmentID string             `json:"enrollment_id,omitempty"`
	State        routing.TeamState  `json:"state,omitempty"`
	LastError    routing.ErrorClass `json:"last_error,omitempty"`
	Entries      []string           `json:"entries,omitempty"`
	Collects     string             `json:"collects"`
	GitHooks     bool               `json:"git_hooks,omitempty"`
	Unreadable   string             `json:"unreadable,omitempty"`
}

// overlapRow is an entry two organizations' ticked teams both cover: Team collects it,
// since its policy was updated more recently, and Other does not.
type overlapRow struct {
	Entry string `json:"entry"`
	Team  string `json:"team"`
	Other string `json:"other"`
}

const (
	listLimit           = 4
	personalCollects    = "everything no ticked organization collects"
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

func (app *App) listTeams(ctx context.Context, cfg *config.Config, st routing.State) teamsView {
	client, err := app.teamsAPIClient(cfg, st.Account)
	return app.listTeamsWithClient(ctx, cfg, st, client, err)
}

func (app *App) listTeamsWithClient(ctx context.Context, cfg *config.Config, st routing.State, client *api.Client, clientErr error) teamsView {
	var v teamsView
	names := loadTeamNames(app.stateDir, cfg.ProfileName)
	var orgs []organization
	if clientErr == nil {
		orgs, clientErr = fetchOrganizations(ctx, client)
	}
	if clientErr != nil {
		v.Unlisted = clientErr.Error()
	}
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
			t := teamRow{ID: p.ID, Name: p.Name, Description: p.Description}
			switch {
			case orgNotAccessibleErr(p.err):
				row.Inaccessible, row.Note, t.Unreadable, t.Collects = true, orgInaccessibleNote, orgInaccessibleNote, "its policy could not be read"
			case p.err != nil:
				t.Unreadable, t.Collects = p.err.Error(), "its policy could not be read"
			default:
				t.Entries, t.Collects, t.GitHooks = p.policy.Repositories, collectsSummary(p.policy, o.Type == "personal"), p.policy.GitHooks
			}
			row.Teams = append(row.Teams, t)
		}
		v.Organizations = append(v.Organizations, row)
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

func (app *App) fetchTeams(ctx context.Context, client *api.Client, authURL string, orgs []organization) []listedOrg {
	out := make([]listedOrg, len(orgs))
	slots := make(chan struct{}, listLimit)
	run := func(fn func() error) error {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			return fn()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var wg sync.WaitGroup
	for i, o := range orgs {
		org := &out[i]
		wg.Go(func() {
			var projects []project
			if org.err = run(func() (err error) { projects, err = fetchProjects(ctx, client, o.ID); return err }); org.err != nil {
				return
			}
			org.teams = make([]listedTeam, len(projects))
			for j, p := range projects {
				team := &org.teams[j]
				team.project = p
				wg.Go(func() {
					team.err = run(func() (err error) { team.policy, err = fetchTeamPolicy(ctx, client, authURL, o.ID, p.ID); return err })
				})
			}
		})
	}
	wg.Wait()
	return out
}

func (app *App) teamsAPIClient(cfg *config.Config, account string) (*api.Client, error) {
	if cfg.APIKey != "" || cfg.ServerKeySignIn || account == sendas.NoPersonLane {
		return nil, errServerKeyTeams
	}
	cred, err := auth.LoadCredential(app.dir, cfg.ProfileName)
	switch {
	case errors.Is(err, auth.ErrNotLoggedIn):
		return nil, errors.New(app.notSignedIn())
	case err != nil:
		return nil, err
	case account != "" && cred.Account() != account:
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

func (v *teamsView) mergeState(st routing.State, names teamNames, now time.Time) {
	for i := range v.Organizations {
		for j := range v.Organizations[i].Teams {
			t := &v.Organizations[i].Teams[j]
			t.Selected, t.EnrollmentID, t.State, t.LastError = false, "", "", ""
		}
	}
	for _, t := range st.Teams {
		org := v.org(t.OrganizationID, names)
		i := slices.IndexFunc(org.Teams, func(r teamRow) bool { return r.ID == t.TeamID })
		if i < 0 {
			org.Teams = append(org.Teams, teamRow{ID: t.TeamID, Name: names.Teams[t.TeamID], Entries: t.Policy.Repositories, Collects: collectsSummary(t.Policy, t.Personal), GitHooks: t.Policy.GitHooks})
			i = len(org.Teams) - 1
		}
		row := &org.Teams[i]
		row.Selected, row.EnrollmentID, row.State, row.LastError = true, t.EnrollmentID, t.StateAt(now), t.LastError
		if t.Personal && org.Type == "" {
			org.Type = "personal"
		}
	}
	v.Overlaps, v.Order = overlapRows(st, names), nil
	if len(v.Overlaps) > 0 {
		for _, t := range routing.Order(st) {
			v.Order = append(v.Order, teamLabel(names, t)+" — "+doctor.PolicyAge(t))
		}
	}
	orgRank := func(o orgRow) int {
		if o.Type == "personal" {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(v.Organizations, func(a, b orgRow) int { return cmp.Compare(orgRank(a), orgRank(b)) })
}

func (v *teamsView) org(id string, names teamNames) *orgRow {
	if i := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == id }); i >= 0 {
		return &v.Organizations[i]
	}
	v.Organizations = append(v.Organizations, orgRow{ID: id, Name: names.Orgs[id], Note: "not listed for you now"})
	return &v.Organizations[len(v.Organizations)-1]
}

// overlapRows are st's policy conflicts, each team named as doctor and setup name it.
func overlapRows(st routing.State, names teamNames) []overlapRow {
	var out []overlapRow
	for _, o := range routing.Overlaps(st) {
		out = append(out, overlapRow{Entry: o.Entry, Team: teamLabel(names, o.Winner), Other: teamLabel(names, o.Other)})
	}
	return out
}

func (v *teamsView) findTeam(orgID, teamID string) *teamRow {
	if oi := slices.IndexFunc(v.Organizations, func(o orgRow) bool { return o.ID == orgID }); oi >= 0 {
		if ti := slices.IndexFunc(v.Organizations[oi].Teams, func(r teamRow) bool { return r.ID == teamID }); ti >= 0 {
			return &v.Organizations[oi].Teams[ti]
		}
	}
	return nil
}

func teamNotFound(err error) bool {
	e, ok := errors.AsType[*api.Error](err)
	return ok && e.ProjectNotFound()
}
func orgNotAccessibleErr(err error) bool {
	e, ok := errors.AsType[*api.Error](err)
	return ok && e.OrgNotAccessible()
}

// teamLabel names t as doctor does: Personal, or its organization and team.
func teamLabel(names teamNames, t routing.Team) string {
	if t.Personal {
		return personalLabel
	}
	return orgTeamLabel(names.Orgs[t.OrganizationID], cmp.Or(names.Teams[t.TeamID], t.TeamID))
}

// orgTeamLabel names an organization's team as doctor does.
func orgTeamLabel(org, team string) string { return doctor.TeamName(org, team) }

type teamNames struct {
	Teams   map[string]string `json:"teams"`
	Orgs    map[string]string `json:"organizations"`
	changed bool
}

func teamNamesPath(stateDir, profile string) (string, bool) {
	if stateDir == "" || profile == "" || profile != filepath.Base(profile) {
		return "", false
	}
	return filepath.Join(stateDir, "team-names", profile+".json"), true
}

func loadTeamNames(stateDir, profile string) teamNames {
	n := teamNames{Teams: map[string]string{}, Orgs: map[string]string{}}
	if path, ok := teamNamesPath(stateDir, profile); ok {
		var raw teamNames
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &raw) == nil {
			maps.Copy(n.Teams, raw.Teams)
			maps.Copy(n.Orgs, raw.Orgs)
		}
	}
	return n
}

func (n *teamNames) set(m map[string]string, id, name string) {
	if id != "" && name != "" && m[id] != name {
		m[id], n.changed = name, true
	}
}
func (n *teamNames) team(id, name string) { n.set(n.Teams, id, name) }
func (n *teamNames) org(id, name string)  { n.set(n.Orgs, id, name) }
func (n teamNames) save(stateDir, profile string) {
	if path, ok := teamNamesPath(stateDir, profile); n.changed && ok && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		if b, err := json.MarshalIndent(n, "", "  "); err == nil {
			_ = config.WriteFileAtomic(path, b, 0o600)
		}
	}
}
