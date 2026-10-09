package cli

import (
	"cmp"
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

type project struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	OrganizationID string `json:"organization_id"`
	CreatedAt      string `json:"created_at,omitempty"`
}

type listProjectsResponse struct {
	Projects []project `json:"projects"`
}

func fetchProjects(ctx context.Context, client *api.Client) ([]project, error) {
	var resp listProjectsResponse
	if err := client.AuthGet(ctx, "/v1/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

var errNoProjects = errors.New("no teams in this organization yet — create one in the Terma app")

// availableProjects fetches the organization's projects, failing with errNoProjects when empty.
func availableProjects(ctx context.Context, client *api.Client) ([]project, error) {
	projects, err := fetchProjects(ctx, client)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return nil, errNoProjects
	}
	return projects, nil
}

// soleOrPick takes the only project without asking, else prompts with current marked.
func soleOrPick(cmd *cobra.Command, projects []project, current string) (*project, error) {
	if len(projects) == 1 {
		return &projects[0], nil
	}
	return pickProject(cmd, projects, current)
}

func matchProject(projects []project, query string) (*project, error) {
	return projectKind.match(projects, query)
}

// pickProject prompts for a project, current marked and kept by a bare Enter.
func pickProject(cmd *cobra.Command, projects []project, current string) (*project, error) {
	labels := projectKind.labels(projects)
	return projectKind.pick(cmd, projects, func(p project) pickRow {
		return pickRow{
			Label:   labels[p.ID],
			Note:    output.Truncate(p.Description, 48),
			Current: p.ID == current,
			Default: p.ID == current || (current == "" && len(projects) == 1),
		}
	})
}

// selectPolicyTeam picks the team whose policy is set up, and returns its name; it never
// creates a telemetry key. --team or TERMA_TEAM_ID is taken as given; the team an earlier
// setup saved is only the picker's default when ask is set and the organization has
// several, since a team created since then is easily the one meant.
func (app *App) selectPolicyTeam(cmd *cobra.Command, cfg *config.Config, ask bool) (string, error) {
	if config.PolicyStub() != "" {
		return "", nil
	}
	saved := ""
	if cfg.ProjectID == "" {
		cfg.ProjectID, saved = cfg.Team, cfg.Team
	}
	client, err := app.newClient(cfg)
	if err != nil {
		return "", err
	}
	projects, err := availableProjects(cmd.Context(), client)
	if err != nil {
		return "", err
	}
	var team *project
	switch {
	case saved != "" && ask && len(projects) > 1:
		team, err = pickProject(cmd, projects, saved)
	case cfg.ProjectID != "":
		team, err = matchProject(projects, cfg.ProjectID)
	default:
		team, err = soleOrPick(cmd, projects, "")
	}
	if err != nil {
		return "", err
	}
	cfg.ProjectID = team.ID
	return cmp.Or(team.Name, team.ID), nil
}

// recordTeamName keeps name as the profile's team id's, for doctor to name; only that, so
// a failure costs nothing but the name.
func (app *App) recordTeamName(profile, id, name string) {
	if name == "" || name == id {
		return
	}
	_ = config.UpdateProfile(app.dir, profile, func(p *config.Profile) {
		if p.Team == id {
			p.TeamName = name
		}
	})
}
