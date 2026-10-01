package cli

import (
	"cmp"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

func (app *App) newLoginCommand() *cobra.Command {
	var noBrowser, force bool
	var label, org string

	cmd := &cobra.Command{
		Use:    "login",
		Short:  "Sign in, reusing the session this machine already has",
		Hidden: true,
		Long: `Signs this machine in against one of your organizations.

A session you already approved is reused: if this profile holds a working credential
for the organization, nothing is minted and no browser opens. Otherwise your browser
opens, you approve the CLI against an organization, and the resulting credential is
stored in ~/.config/terma/credentials.json.

The credential is scoped to the organization, not a team, so you can switch
teams afterwards without signing in again. Each organization you sign into keeps
its own credential in the profile; ` + "`terma org use`" + ` switches between them.

  --org <name-or-id>   sign into (or switch to) a particular organization
  --force              always open the browser and mint a new session; the session it
                       replaces for that organization is revoked`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}
			res, err := app.signIn(cmd, cfg, signInOptions{
				org:       parseOrgRef(org),
				force:     force,
				noBrowser: noBrowser,
				label:     label,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if res.reused {
				fmt.Fprintf(out, "%s (existing session reused).\n", res.signedInAs())
			} else {
				fmt.Fprintf(out, "\n%s.\n", res.signedInAs())
			}
			fmt.Fprintln(out, "Next: run `terma install` inside a repository.")
			return nil
		},
	}

	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the authorization URL instead of opening a browser")
	cmd.Flags().StringVar(&label, "label", "", "name shown for this session (defaults to the hostname)")
	cmd.Flags().StringVar(&org, "org", "", "organization to sign into, by name or id (default: the current one)")
	cmd.Flags().BoolVar(&force, "force", false, "mint a new session even if a working one is stored")
	return cmd
}

func (app *App) newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "logout",
		Short:  "Revoke this machine's credentials",
		Hidden: true,
		Long: `Revokes every session this profile holds server-side and deletes the local
credentials — one per organization you signed into.

Revoking server-side is what makes this meaningful: deleting the local file alone
would leave live tokens that anyone holding a copy could keep using.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.loadConfig()
			if err != nil {
				return err
			}
			if cfg.APIKey != "" {
				return errors.New("TERMA_API_KEY is set — there is no session to log out of; unset it to use the stored credential")
			}

			creds, err := auth.Credentials(cfg.ProfileName)
			if err != nil {
				return err
			}
			if len(creds) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Already logged out.")
				return nil
			}
			// A failed revoke still clears the local file: the user asked to be logged out.
			for _, cred := range creds {
				if err := app.revokeSession(cmd.Context(), cfg, cred); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not revoke the session for %s server-side (%v).\n",
						cmp.Or(cred.OrganizationID, "this organization"), err)
				}
			}
			if err := auth.DeleteCredential(cfg.ProfileName); err != nil {
				return err
			}
			if len(creds) > 1 {
				fmt.Fprintf(cmd.OutOrStdout(), "Logged out of %d organizations.\n", len(creds))
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			}
			return nil
		},
	}
}

type identityResponse struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	AuthType       string `json:"auth_type"`
	UserID         string `json:"user_id"`
	Email          string `json:"email"`
}

func (app *App) newWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "whoami",
		Short:  "Show the identity and scope of the current credential",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, format, err := app.setupCommand(resolveRepoProject)
			if err != nil {
				return err
			}

			var identity identityResponse
			if err := client.AuthGet(cmd.Context(), "/v1/whoami", nil, &identity); err != nil {
				return err
			}

			pairs := [][2]string{
				{"profile", cfg.ProfileName},
				{"api", cfg.APIURL},
				{"auth endpoint", cfg.AuthURL},
				{"auth", identity.AuthType},
			}
			if identity.Email != "" {
				pairs = append(pairs, [2]string{"user", identity.Email})
			} else if identity.UserID != "" {
				pairs = append(pairs, [2]string{"user", identity.UserID})
			}
			pairs = append(pairs, [2]string{"organization", cmp.Or(cfg.OrganizationName, identity.OrganizationID)})
			if cfg.ProjectID != "" {
				pairs = append(pairs, [2]string{"team", cmp.Or(cfg.ProjectName, cfg.ProjectID)})
			} else {
				pairs = append(pairs, [2]string{"team", "(no repository team)"})
			}
			if cfg.APIKey == "" {
				if creds, err := auth.Credentials(cfg.ProfileName); err == nil && len(creds) > 1 {
					pairs = append(pairs, [2]string{"also signed in", fmt.Sprintf("%d other organization(s) — see `terma org list`", len(creds)-1)})
				}
			}

			return output.KeyValues(cmd.OutOrStdout(), format, pairs, identity)
		},
	}
}
