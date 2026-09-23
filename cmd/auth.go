package cmd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/output"
)

func newLoginCommand() *cobra.Command {
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
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			res, err := signIn(cmd, cfg, signInOptions{
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

func newLogoutCommand() *cobra.Command {
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
			cfg, err := loadConfig()
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
			// A failed revoke must not strand the local credential — the user asked to be
			// logged out, so report it and still clear the file.
			for _, cred := range creds {
				if err := revokeSession(cmd.Context(), cfg, cred); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not revoke the session for %s server-side (%v).\n",
						firstNonEmpty(cred.OrganizationID, "this organization"), err)
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

func newWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "whoami",
		Short:  "Show the identity and scope of the current credential",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, format, err := setupCommand(resolveRepoProject)
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
			pairs = append(pairs, [2]string{"organization", nameOrID(cfg.OrganizationName, identity.OrganizationID)})
			// whoami describes the credential, which is org-scoped; the project
			// comes from the repository binding or a one-command override.
			if cfg.ProjectID != "" {
				pairs = append(pairs, [2]string{"project", nameOrID(cfg.ProjectName, cfg.ProjectID)})
			} else {
				pairs = append(pairs, [2]string{"project", "(no repository project)"})
			}
			// Other organizations this profile can switch to without a browser.
			if cfg.APIKey == "" {
				if creds, err := auth.Credentials(cfg.ProfileName); err == nil && len(creds) > 1 {
					pairs = append(pairs, [2]string{"also signed in", fmt.Sprintf("%d other organization(s) — see `terma org list`", len(creds)-1)})
				}
			}

			return output.KeyValues(cmd.OutOrStdout(), format, pairs, identity)
		},
	}
}

// orgRef is how a user names an organization on the command line: an id, or a name to
// match against the organizations they belong to.
type orgRef struct {
	ID   string
	Name string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseOrgRef reads an argument. Ids are UUIDs; anything else is taken as a name.
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

// matches reports whether an organization is the one referred to.
func (r orgRef) matches(id, name string) bool {
	if r.ID != "" {
		return r.ID == id
	}
	return r.Name != "" && strings.EqualFold(r.Name, name)
}

// hint is what the browser page is told to preselect.
func (r orgRef) hint() string { return firstNonEmpty(r.ID, r.Name) }

func (r orgRef) String() string { return firstNonEmpty(r.Name, r.ID) }

type signInOptions struct {
	// org is the organization to sign into. Empty means whichever is active, or
	// whatever the user picks in the browser.
	org orgRef
	// force skips reuse: the browser opens and a new session is minted.
	force     bool
	noBrowser bool
	label     string
}

type signInResult struct {
	cred    *auth.Credential
	orgName string
	// reused is true when a stored session served, and no browser opened.
	reused bool
}

// signedInAs is the one sentence that says who is signed in and where, without its
// full stop so a caller can qualify it.
func (r *signInResult) signedInAs() string {
	return fmt.Sprintf("Signed in as %s in %s", firstNonEmpty(r.cred.UserEmail, "your account"),
		nameOrID(r.orgName, r.cred.OrganizationID))
}

// signInAndReload signs in and returns the configuration as the sign-in left
// it: signing in points the profile at the credential's organization, so the
// configuration loaded before it is stale.
func signInAndReload(cmd *cobra.Command, cfg *config.Config, opts signInOptions) (*config.Config, error) {
	_, err := signIn(cmd, cfg, opts)
	if err != nil {
		return nil, err
	}
	return loadConfig()
}

// signIn is the one way a command obtains a credential. It prefers a session the
// profile already holds — verified against the auth host, so a revoked or expired one
// is not handed back — and opens the browser only when there is none for the
// organization asked for. Every path ends with the credential active in the store and
// the profile pointed at its organization.
func signIn(cmd *cobra.Command, cfg *config.Config, opts signInOptions) (*signInResult, error) {
	if cfg.APIKey != "" {
		return nil, errors.New("TERMA_API_KEY is set — unset it to sign in as a user, or keep using the server key")
	}
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()

	want := opts.org
	if !opts.force {
		// A name has to become an id to find a stored credential; that needs any
		// working credential to list the organizations with. Without one the browser
		// resolves it — the page preselects by name as well as by id.
		if want.ID == "" && want.Name != "" {
			if org, err := resolveOrganization(ctx, cfg, want); err == nil {
				want = orgRef{ID: org.ID, Name: org.Name}
			} else if !errors.Is(err, errNoWorkingCredential) {
				return nil, err
			}
		}
		if res, ok, err := reuseStoredSession(ctx, cfg, want); err != nil {
			return nil, err
		} else if ok {
			return res, nil
		}
	}

	client := api.NewAnonymous(cfg.AuthURL, Version)
	cred, err := auth.Login(ctx, client, auth.LoginOptions{
		AppURL:       cfg.AppURL,
		Label:        firstNonEmpty(opts.label, auth.DefaultLabel()),
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
			nameOrID(orgName, cred.OrganizationID), want, nameOrID(orgName, cred.OrganizationID))
	}

	replaced, err := auth.SaveCredential(cfg.ProfileName, cred)
	if err != nil {
		return nil, err
	}
	// The session this one supersedes is revoked so a re-login does not leave a trail
	// of live sessions. Best-effort: it may already be dead, which is often why the
	// user is here.
	if replaced != nil {
		_ = revokeSession(ctx, cfg, replaced)
	}
	if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) { applyLogin(p, cred, orgName) }); err != nil {
		return nil, err
	}
	return &signInResult{cred: cred, orgName: orgName}, nil
}

// reuseStoredSession tries the credential the profile holds for the organization asked
// for (the active one when none was), verifies it still works, and makes it active. A
// dead credential is dropped so it is not tried again. ok is false when there is
// nothing usable and the caller should open the browser.
func reuseStoredSession(ctx context.Context, cfg *config.Config, want orgRef) (*signInResult, bool, error) {
	var cred *auth.Credential
	var err error
	switch {
	case want.ID != "":
		cred, err = auth.LoadCredentialFor(cfg.ProfileName, want.ID)
	case want.Name != "":
		// Unresolved name: nothing stored can be known to match.
		return nil, false, nil
	default:
		cred, err = auth.LoadCredential(cfg.ProfileName)
	}
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.RefreshToken == "" {
		return nil, false, nil
	}

	verified, identity, err := verifyCredential(ctx, cfg, cred)
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
		// Switching to a parked credential whose name the profile no longer holds:
		// one listing names it. Not fatal if it cannot.
		if org, err := lookupOrganization(ctx, cfg, verified, verified.OrganizationID); err == nil {
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

// verifyCredential asks the auth host who the credential is, refreshing it if the
// access token is spent. It returns the credential as it now stands — possibly a
// rotated pair, already persisted for its organization.
func verifyCredential(ctx context.Context, cfg *config.Config, cred *auth.Credential) (*auth.Credential, identityResponse, error) {
	client, err := api.New(cfg, api.Options{Version: Version, Credential: cred})
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

// errNoWorkingCredential means no stored credential could reach the auth host, so
// nothing that needs one — listing organizations, resolving a name — is possible.
var errNoWorkingCredential = errors.New("no working credential")

// workingClient returns a client on the first stored credential that still works,
// the active one first. It is how a command lists organizations before it knows which
// one it wants.
func workingClient(ctx context.Context, cfg *config.Config) (*api.Client, error) {
	if cfg.APIKey != "" {
		return newClient(cfg)
	}
	creds, err := auth.Credentials(cfg.ProfileName)
	if err != nil {
		return nil, err
	}
	for _, cred := range creds {
		if cred.CheckEnvironment(cfg.AuthURL) != nil {
			continue
		}
		client, err := api.New(cfg, api.Options{Version: Version, Credential: cred})
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

// resolveOrganization turns a reference into one of the organizations the user
// belongs to, using whichever stored credential still works.
func resolveOrganization(ctx context.Context, cfg *config.Config, ref orgRef) (*organization, error) {
	client, err := workingClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	orgs, err := fetchOrganizations(ctx, client)
	if err != nil {
		return nil, err
	}
	return matchOrganization(orgs, ref.String())
}

// lookupOrganization names an organization by id using a specific credential.
func lookupOrganization(ctx context.Context, cfg *config.Config, cred *auth.Credential, id string) (*organization, error) {
	client, err := api.New(cfg, api.Options{Version: Version, Credential: cred})
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

// matchOrganization resolves an argument the way every named thing resolves; see
// matchKind.index.
func matchOrganization(orgs []organization, query string) (*organization, error) {
	return organizationKind.match(orgs, query)
}

// revokeSession ends one stored session server-side.
func revokeSession(ctx context.Context, cfg *config.Config, cred *auth.Credential) error {
	client, err := api.New(cfg, api.Options{Version: Version, Credential: cred})
	if err != nil {
		return err
	}
	return client.RevokeSession(ctx)
}

// applyLogin records the account scope. Project selection belongs to repositories.
func applyLogin(p *config.Profile, cred *auth.Credential, orgName string) {
	p.SelectOrganization(cred.OrganizationID, orgName)
}

// nameOrID uses the human name for terminal output, falling back to the ID when
// no name is known. Structured output retains IDs for scripts and diagnostics.
func nameOrID(name, id string) string {
	return firstNonEmpty(name, id)
}
