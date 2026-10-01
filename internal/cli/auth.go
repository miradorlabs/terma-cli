package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
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

The credential is scoped to the organization, not a project, so you can switch
projects afterwards without signing in again. Each organization you sign into keeps
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
				pairs = append(pairs, [2]string{"project", cmp.Or(cfg.ProjectName, cfg.ProjectID)})
			} else {
				pairs = append(pairs, [2]string{"project", "(no repository project)"})
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

type orgRef struct {
	ID   string
	Name string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseOrgRef takes a UUID as an id and anything else as a name.
func parseOrgRef(arg string) orgRef {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return orgRef{}
	}
	if uuidPattern.MatchString(arg) {
		return orgRef{ID: arg}
	}
	return orgRef{Name: arg}
}

func (r orgRef) empty() bool { return r.ID == "" && r.Name == "" }

func (r orgRef) matches(id, name string) bool {
	if r.ID != "" {
		return r.ID == id
	}
	return r.Name != "" && strings.EqualFold(r.Name, name)
}

// hint is what the browser page is told to preselect.
func (r orgRef) hint() string { return cmp.Or(r.ID, r.Name) }

func (r orgRef) String() string { return cmp.Or(r.Name, r.ID) }

type signInOptions struct {
	// org empty means whichever is active, or whatever the user picks in the browser.
	org                orgRef
	force              bool
	noBrowser          bool
	label              string
	pauseBeforeBrowser bool
}

type signInResult struct {
	cred    *auth.Credential
	orgName string
	reused  bool
}

// signedInAs has no full stop so a caller can qualify it.
func (r *signInResult) signedInAs() string {
	return fmt.Sprintf("Signed in as %s in %s", cmp.Or(r.cred.UserEmail, "your account"),
		cmp.Or(r.orgName, r.cred.OrganizationID))
}

// signInAndReload reloads because signing in points the profile at the credential's
// organization, so the configuration loaded before it is stale.
func (app *App) signInAndReload(cmd *cobra.Command, cfg *config.Config, opts signInOptions) (*config.Config, error) {
	_, err := app.signIn(cmd, cfg, opts)
	if err != nil {
		return nil, err
	}
	return app.loadConfig()
}

// signIn is the one way a command obtains a credential: a stored session verified
// against the auth host, else the browser.
func (app *App) signIn(cmd *cobra.Command, cfg *config.Config, opts signInOptions) (*signInResult, error) {
	if cfg.APIKey != "" {
		return nil, errors.New("TERMA_API_KEY is set — unset it to sign in as a user, or keep using the server key")
	}
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()

	want := opts.org
	if !opts.force {
		// A name must become an id to find a stored credential; without a working
		// credential to list organizations, the browser page resolves the name.
		if want.ID == "" && want.Name != "" {
			if org, err := app.resolveOrganization(ctx, cfg, want); err == nil {
				want = orgRef{ID: org.ID, Name: org.Name}
			} else if !errors.Is(err, errNoWorkingCredential) {
				return nil, err
			}
		}
		if res, ok, err := app.reuseStoredSession(ctx, cfg, want); err != nil {
			return nil, err
		} else if ok {
			return res, nil
		}
	}

	if opts.pauseBeforeBrowser && !opts.noBrowser && canPrompt() {
		if err := waitForBrowserEnter(cmd); err != nil {
			return nil, err
		}
	}

	client := api.NewAnonymous(cfg.AuthURL, app.version)
	cred, err := auth.Login(ctx, client, auth.LoginOptions{
		AppURL:       cfg.AppURL,
		Label:        cmp.Or(opts.label, auth.DefaultLabel()),
		Organization: want.hint(),
		NoBrowser:    opts.noBrowser,
		Out:          errOut,
	})
	if err != nil {
		return nil, err
	}
	orgName := ""
	if result := client.LastLogin(); result != nil {
		orgName = result.OrganizationName
	}
	if !want.empty() && !want.matches(cred.OrganizationID, orgName) {
		fmt.Fprintf(errOut, "Note: you approved %s in the browser rather than %s. Signed in to %s.\n",
			cmp.Or(orgName, cred.OrganizationID), want, cmp.Or(orgName, cred.OrganizationID))
	}

	replaced, err := auth.SaveCredential(cfg.ProfileName, cred)
	if err != nil {
		return nil, err
	}
	// Best-effort: the superseded session may already be dead, often why the user is here.
	if replaced != nil {
		_ = app.revokeSession(ctx, cfg, replaced)
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { applyLogin(p, cred, orgName) }); err != nil {
		return nil, err
	}
	return &signInResult{cred: cred, orgName: orgName}, nil
}

// reuseStoredSession drops a dead credential so it is not tried again; ok false means
// open the browser.
func (app *App) reuseStoredSession(ctx context.Context, cfg *config.Config, want orgRef) (*signInResult, bool, error) {
	var cred *auth.Credential
	var err error
	switch {
	case want.ID != "":
		cred, err = auth.LoadCredentialFor(cfg.ProfileName, want.ID)
	case want.Name != "":
		return nil, false, nil
	default:
		cred, err = auth.LoadCredential(cfg.ProfileName)
	}
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.RefreshToken == "" {
		return nil, false, nil
	}

	verified, identity, err := app.verifyCredential(ctx, cfg, cred)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		_ = auth.DeleteCredentialFor(cfg.ProfileName, cred.OrganizationID)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, err := auth.UseOrganization(cfg.ProfileName, verified.OrganizationID); err != nil {
		return nil, false, err
	}
	orgName := ""
	if want.ID == verified.OrganizationID {
		orgName = want.Name
	}
	if orgName == "" && cfg.OrganizationID == verified.OrganizationID {
		orgName = cfg.OrganizationName
	}
	if orgName == "" {
		if org, err := app.lookupOrganization(ctx, cfg, verified, verified.OrganizationID); err == nil {
			orgName = org.Name
		}
	}
	if identity.Email != "" && verified.UserEmail == "" {
		verified.UserEmail = identity.Email
		_ = auth.UpdateCredential(cfg.ProfileName, verified)
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { applyLogin(p, verified, orgName) }); err != nil {
		return nil, false, err
	}
	return &signInResult{cred: verified, orgName: orgName, reused: true}, true, nil
}

// verifyCredential returns the credential as it now stands: possibly a rotated pair,
// already persisted for its organization.
func (app *App) verifyCredential(ctx context.Context, cfg *config.Config, cred *auth.Credential) (*auth.Credential, identityResponse, error) {
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return nil, identityResponse{}, err
	}
	var identity identityResponse
	if err := client.AuthGet(ctx, "/v1/whoami", nil, &identity); err != nil {
		return nil, identityResponse{}, err
	}
	current := client.Credential()
	if current.OrganizationID == "" {
		current.OrganizationID = identity.OrganizationID
	}
	return current, identity, nil
}

var errNoWorkingCredential = errors.New("no working credential")

func (app *App) workingClient(ctx context.Context, cfg *config.Config) (*api.Client, error) {
	if cfg.APIKey != "" {
		return app.newClient(cfg)
	}
	creds, err := auth.Credentials(cfg.ProfileName)
	if err != nil {
		return nil, err
	}
	for _, cred := range creds {
		if cred.CheckEnvironment(cfg.AuthURL) != nil {
			continue
		}
		client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
		if err != nil {
			continue
		}
		var identity identityResponse
		if err := client.AuthGet(ctx, "/v1/whoami", nil, &identity); err != nil {
			if errors.Is(err, auth.ErrNotLoggedIn) {
				continue
			}
			return nil, err
		}
		return client, nil
	}
	return nil, errNoWorkingCredential
}

func fetchOrganizations(ctx context.Context, client *api.Client) ([]organization, error) {
	var resp listOrganizationsResponse
	if err := client.AuthGet(ctx, "/v1/organizations", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Organizations, nil
}

func (app *App) resolveOrganization(ctx context.Context, cfg *config.Config, ref orgRef) (*organization, error) {
	client, err := app.workingClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	orgs, err := fetchOrganizations(ctx, client)
	if err != nil {
		return nil, err
	}
	return matchOrganization(orgs, ref.String())
}

func (app *App) lookupOrganization(ctx context.Context, cfg *config.Config, cred *auth.Credential, id string) (*organization, error) {
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return nil, err
	}
	orgs, err := fetchOrganizations(ctx, client)
	if err != nil {
		return nil, err
	}
	for i := range orgs {
		if orgs[i].ID == id {
			return &orgs[i], nil
		}
	}
	return nil, fmt.Errorf("organization %s not found", id)
}

func matchOrganization(orgs []organization, query string) (*organization, error) {
	return organizationKind.match(orgs, query)
}

func (app *App) revokeSession(ctx context.Context, cfg *config.Config, cred *auth.Credential) error {
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return err
	}
	return client.RevokeSession(ctx)
}

// applyLogin records only the account scope: project selection belongs to repositories.
func applyLogin(p *config.Profile, cred *auth.Credential, orgName string) {
	p.SelectOrganization(cred.OrganizationID, orgName)
}

func waitForBrowserEnter(cmd *cobra.Command) error {
	fmt.Fprint(cmd.ErrOrStderr(), "Press Enter to open your browser and sign in to Terma (Ctrl-C to cancel): ")
	// Do not buffer input: queued answers belong to the prompts that follow.
	var key [1]byte
	for {
		n, err := cmd.InOrStdin().Read(key[:])
		if n > 0 && key[0] == '\n' {
			return cmd.Context().Err()
		}
		if err != nil {
			return fmt.Errorf("read browser confirmation: %w", err)
		}
	}
}
