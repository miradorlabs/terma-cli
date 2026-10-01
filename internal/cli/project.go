package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"

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

func (app *App) newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects"},
		Short:   "List projects and show the repository's binding",
		Hidden:  true,
		Long: `Projects are selected per repository by terma install.
Read commands use the current repository's binding, or an explicit --project override.`,
	}
	cmd.AddCommand(app.newProjectListCommand(), app.newProjectShowCommand())
	return cmd
}

func (app *App) newProjectListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List projects in the current organization",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, format, err := app.setupCommand(resolveRepoProject)
			if err != nil {
				return err
			}

			projects, err := fetchProjects(cmd.Context(), client)
			if err != nil {
				return err
			}

			rows := make([][]string, 0, len(projects))
			labels := projectKind.labels(projects)
			for _, p := range projects {
				marker := " "
				if p.ID == cfg.ProjectID {
					marker = "*"
				}
				rows = append(rows, []string{marker, labels[p.ID], output.Truncate(p.Description, 48)})
			}

			return output.Render(cmd.OutOrStdout(), format, output.Table{
				Headers: []string{"", "NAME", "DESCRIPTION"},
				Rows:    rows,
			}, listProjectsResponse{Projects: projects})
		},
	}
}

func (app *App) newProjectShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "show",
		Short:  "Show this repository's project",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.loadProjectConfig()
			if err != nil {
				return err
			}
			format, err := app.resolveFormat()
			if err != nil {
				return err
			}
			if cfg.ProjectID == "" {
				return fmt.Errorf("no project bound to this repository — run `terma install` or pass --project")
			}

			organizationID := cmp.Or(cfg.ProjectOrganizationID, cfg.OrganizationID)
			organizationName := cfg.OrganizationName
			if organizationID != cfg.OrganizationID {
				organizationName = ""
			}
			current := project{ID: cfg.ProjectID, Name: cfg.ProjectName, OrganizationID: organizationID}
			return output.KeyValues(cmd.OutOrStdout(), format, [][2]string{
				{"name", cmp.Or(cfg.ProjectName, cfg.ProjectID)},
				{"organization", cmp.Or(organizationName, organizationID)},
				{"profile", cfg.ProfileName},
			}, current)
		},
	}
}

func fetchProjects(ctx context.Context, client *api.Client) ([]project, error) {
	var resp listProjectsResponse
	if err := client.AuthGet(ctx, "/v1/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

var errNoProjects = errors.New("no projects in this organization yet — create one in the Terma app")

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
