package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/output"
)

type organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
}

type listOrganizationsResponse struct {
	Organizations []organization `json:"organizations"`
}

func newOrgCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "org",
		Aliases: []string{"orgs", "organization", "organizations"},
		Short:   "List and switch between your organizations",
		Long: `A credential is issued against one organization. This profile keeps one for
each organization you have signed into, so ` + "`terma org use`" + ` switches between them
without a browser; an organization you have not signed into yet opens one, with
that organization preselected.

Projects are selected per repository with ` + "`terma install`" + `. Switching
organizations does not change repository bindings.`,
	}
	cmd.AddCommand(newOrgListCommand(), newOrgUseCommand())
	return cmd
}

func newOrgListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List organizations you belong to",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, format, err := setupCommand()
			if err != nil {
				return err
			}

			orgs, err := fetchOrganizations(cmd.Context(), client)
			if err != nil {
				return err
			}
			signedIn := storedOrganizations(cfg)

			rows := make([][]string, 0, len(orgs))
			labels := organizationKind.labels(orgs)
			for _, o := range orgs {
				marker := " "
				if o.ID == cfg.OrganizationID {
					marker = "*"
				}
				session := ""
				if signedIn[o.ID] {
					session = "yes"
				}
				rows = append(rows, []string{marker, labels[o.ID], o.Role, session})
			}

			return output.Render(cmd.OutOrStdout(), format, output.Table{
				Headers: []string{"", "NAME", "ROLE", "SIGNED IN"},
				Rows:    rows,
			}, listOrganizationsResponse{Organizations: orgs})
		},
	}
}

func newOrgUseCommand() *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{
		Use:   "use [name-or-id]",
		Short: "Switch the organization the CLI works in",
		Long: `Makes an organization the current one. A session this profile already holds for
it is reused and verified; otherwise your browser opens with the organization
preselected. With no argument on a terminal it presents a picker.

Choose a project for each repository with ` + "`terma install`" + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.APIKey != "" {
				return errors.New("TERMA_API_KEY is set — a server key is bound to one project; unset it to switch organizations")
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			var want orgRef
			if len(args) == 1 {
				want = parseOrgRef(args[0])
			} else {
				client, err := workingClient(ctx, cfg)
				if errors.Is(err, errNoWorkingCredential) {
					return fmt.Errorf("%w: sign in first with `terma login`, or name the organization: `terma org use <name-or-id>`", auth.ErrNotLoggedIn)
				}
				if err != nil {
					return err
				}
				orgs, err := fetchOrganizations(ctx, client)
				if err != nil {
					return err
				}
				if len(orgs) == 0 {
					return errors.New("you do not belong to any organization yet")
				}
				chosen, err := pickOrganization(cmd, cfg, orgs)
				if err != nil {
					return err
				}
				want = orgRef{ID: chosen.ID, Name: chosen.Name}
			}

			before := cfg.OrganizationID
			res, err := signIn(cmd, cfg, signInOptions{org: want, noBrowser: noBrowser})
			if err != nil {
				return err
			}
			where := nameOrID(res.orgName, res.cred.OrganizationID)
			switch {
			case res.reused && before == res.cred.OrganizationID:
				fmt.Fprintf(out, "Already using %s.\n", where)
			case res.reused:
				fmt.Fprintf(out, "Now using %s (existing session reused).\n", where)
			default:
				fmt.Fprintf(out, "\nNow using %s.\n", where)
			}

			fmt.Fprintln(out, "Projects are selected per repository with `terma install`.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the authorization URL instead of opening a browser, if a sign-in is needed")
	return cmd
}

// storedOrganizations reports which organizations the profile holds a credential for.
func storedOrganizations(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	if cfg.APIKey != "" {
		return out
	}
	creds, err := auth.Credentials(cfg.ProfileName)
	if err != nil {
		return out
	}
	for _, c := range creds {
		if c.CheckEnvironment(cfg.AuthURL) == nil {
			out[c.OrganizationID] = true
		}
	}
	return out
}

func pickOrganization(cmd *cobra.Command, cfg *config.Config, orgs []organization) (*organization, error) {
	signedIn := storedOrganizations(cfg)
	labels := organizationKind.labels(orgs)
	return organizationKind.pick(cmd, orgs, func(o organization) pickRow {
		note := o.Role
		if signedIn[o.ID] {
			note += "  signed in"
		}
		return pickRow{Label: labels[o.ID], Note: note, Current: o.ID == cfg.OrganizationID}
	})
}
