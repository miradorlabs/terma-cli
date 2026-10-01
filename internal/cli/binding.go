package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/install"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/ui/spinner"
)

func boundTo(p *project, cfg *config.Config) install.Binding {
	return install.Binding{ID: p.ID, Name: p.Name, OrganizationID: p.OrganizationID, Environment: nonProd(cfg.Environment)}
}

// resolveBinding with verify checks the binding against the credential's projects: one
// from another environment or organization would be refused at the first key mint.
func (app *App) resolveBinding(cmd *cobra.Command, cfg *config.Config, existing *termaproject.File, ref string, verify, ask bool) (install.Binding, error) {
	sp := spinner.New(cmd.ErrOrStderr())
	sp.Start("Loading projects…")
	defer sp.Stop()
	ref = strings.TrimSpace(ref)
	if serverkey.Is(cfg.APIKey) {
		return app.serverKeyBinding(cmd.Context(), cfg, existing, ref)
	}
	current := ""
	if existing != nil {
		current = existing.Project.ID
	}
	if ref != "" {
		client, err := app.newClient(cfg)
		if err != nil {
			if errors.Is(err, auth.ErrNotLoggedIn) {
				return install.Binding{ID: ref, Environment: nonProd(cfg.Environment)}, nil
			}
			return install.Binding{}, err
		}
		projects, err := fetchProjects(cmd.Context(), client)
		sp.Stop()
		if err != nil {
			return install.Binding{}, err
		}
		if p, err := matchProject(projects, ref); err == nil {
			return boundTo(p, cfg), nil
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "No project matches %q in this organization — pick one:\n", ref)
		p, err := pickProject(cmd, projects, current)
		if err != nil {
			return install.Binding{}, err
		}
		return boundTo(p, cfg), nil
	}
	if existing != nil && !verify {
		return install.Kept(existing), nil
	}

	client, err := app.newClient(cfg)
	if err != nil {
		if existing != nil && errors.Is(err, auth.ErrNotLoggedIn) {
			return install.Kept(existing), nil
		}
		return install.Binding{}, err
	}
	projects, err := availableProjects(cmd.Context(), client)
	sp.Stop()
	if errors.Is(err, errNoProjects) {
		return install.Binding{}, fmt.Errorf("%w, then run `terma install` again", err)
	}
	if err != nil {
		return install.Binding{}, err
	}
	bound := existing != nil && slices.ContainsFunc(projects, func(p project) bool { return p.ID == current })

	switch {
	case existing != nil && !bound && !ask:
		return install.Binding{}, fmt.Errorf("%s — run `terma install --project <name or id>` with one of yours (`terma project list` lists them)", unreachableBinding(existing, cfg))
	case existing != nil && !bound:
		reason := unreachableBinding(existing, cfg)
		fmt.Fprintf(cmd.ErrOrStderr(), "%s%s.\n", strings.ToUpper(reason[:1]), reason[1:])
		current = ""
	case bound && !ask:
		return install.Kept(existing), nil
	}

	p, err := soleOrPick(cmd, projects, current)
	if err != nil {
		return install.Binding{}, err
	}
	if bound && p.ID == current {
		return install.Kept(existing), nil
	}
	return boundTo(p, cfg), nil
}

func unreachableBinding(existing *termaproject.File, cfg *config.Config) string {
	p := existing.Project
	org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID)
	if org == "" {
		org = "your organization"
	}
	msg := fmt.Sprintf("this repository is bound to %s, which is not a project in %s", cmp.Or(p.Name, p.ID), org)
	switch {
	case !config.SameAccounts(p.Environment, cfg.Environment):
		return fmt.Sprintf("%s: it was bound in %s, and terma is using %s", msg, environmentLabel(p.Environment), environmentLabel(cfg.Environment))
	case p.OrganizationID != "" && p.OrganizationID != cfg.OrganizationID:
		return fmt.Sprintf("%s: it belongs to organization %s (`terma org use %s` switches to it, if you are a member)", msg, p.OrganizationID, p.OrganizationID)
	}
	return msg
}

func environmentLabel(env string) string {
	if env == "" || env == config.EnvProd {
		return "production"
	}
	return "the " + env + " environment"
}

// serverKeyBinding asks the gateway's /v1/identity, since the account service lists
// projects only for a signed-in user; another project is refused, as the key cannot
// deliver its events.
func (app *App) serverKeyBinding(ctx context.Context, cfg *config.Config, existing *termaproject.File, ref string) (install.Binding, error) {
	client, err := app.newClient(cfg)
	if err != nil {
		return install.Binding{}, err
	}
	var identity struct {
		ProjectID      string `json:"project_id"`
		OrganizationID string `json:"organization_id"`
	}
	if err := client.Get(ctx, "/v1/identity", nil, &identity); err != nil {
		return install.Binding{}, fmt.Errorf("look up the project TERMA_API_KEY belongs to: %w", err)
	}
	if identity.ProjectID == "" {
		return install.Binding{}, errors.New("TERMA_API_KEY names no project")
	}
	if ref != "" && ref != identity.ProjectID {
		return install.Binding{}, fmt.Errorf("TERMA_API_KEY belongs to project %s, not %q — a server key binds only its own project, named by id", identity.ProjectID, ref)
	}
	b := install.Binding{ID: identity.ProjectID, OrganizationID: identity.OrganizationID, Environment: nonProd(cfg.Environment)}
	if existing != nil {
		if existing.Project.ID != identity.ProjectID {
			return install.Binding{}, fmt.Errorf("this repository is bound to project %s, and TERMA_API_KEY belongs to %s", existing.Project.ID, identity.ProjectID)
		}
		b.Name = existing.Project.Name
	}
	return b, nil
}

func nonProd(env string) string {
	if env == config.EnvProd {
		return ""
	}
	return env
}
