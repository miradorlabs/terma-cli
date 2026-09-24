package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/output"
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

func newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects"},
		Short:   "List projects and show the repository's binding",
		// Advanced: install owns project selection and repository binding.
		Hidden: true,
		Long: `Projects are selected per repository by terma install.
Read commands use the current repository's binding, or an explicit --project override.`,
	}
	cmd.AddCommand(newProjectListCommand(), newProjectShowCommand())
	return cmd
}

func newProjectListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List projects in the current organization",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, format, err := setupCommand(resolveRepoProject)
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

func newProjectShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "show",
		Short:  "Show this repository's project",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return err
			}
			format, err := resolveFormat()
			if err != nil {
				return err
			}
			if cfg.ProjectID == "" {
				return fmt.Errorf("no project bound to this repository — run `terma install` or pass --project")
			}

			organizationID := firstNonEmpty(cfg.ProjectOrganizationID, cfg.OrganizationID)
			organizationName := cfg.OrganizationName
			if organizationID != cfg.OrganizationID {
				organizationName = ""
			}
			current := project{ID: cfg.ProjectID, Name: cfg.ProjectName, OrganizationID: organizationID}
			return output.KeyValues(cmd.OutOrStdout(), format, [][2]string{
				{"name", nameOrID(cfg.ProjectName, cfg.ProjectID)},
				{"organization", nameOrID(organizationName, organizationID)},
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

// errNoProjects is the one thing terma says about an organization with nothing to
// select, whichever command found out.
var errNoProjects = errors.New("no projects in this organization yet — create one in the Terma app")

// availableProjects fetches the organization's projects and fails with errNoProjects
// when there are none, so no caller has to word the empty case itself.
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

// soleOrPick takes the only project without asking, and prompts among several.
func soleOrPick(cmd *cobra.Command, projects []project) (*project, error) {
	if len(projects) == 1 {
		return &projects[0], nil
	}
	return pickProject(cmd, projects)
}

// matchProject resolves an argument the way every named thing resolves; see
// matchKind.index.
func matchProject(projects []project, query string) (*project, error) {
	return projectKind.match(projects, query)
}

// pickProject prompts for a selection on a terminal.
func pickProject(cmd *cobra.Command, projects []project) (*project, error) {
	cfg, _ := loadProjectConfig()
	labels := projectKind.labels(projects)
	return projectKind.pick(cmd, projects, func(p project) pickRow {
		return pickRow{Label: labels[p.ID], Note: output.Truncate(p.Description, 48), Current: cfg != nil && p.ID == cfg.ProjectID}
	})
}
