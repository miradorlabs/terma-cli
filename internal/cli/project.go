package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
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
