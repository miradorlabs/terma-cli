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
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

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
	noBrowser          bool
	pauseBeforeBrowser bool
}

type signInResult struct {
	cred    *auth.Credential
	orgName string
	reused  bool
}

// orgAsker picks one of orgs, current marked; nil keeps the current one without asking.
type orgAsker func(orgs []organization, current string) (*organization, error)

// setupSignIn signs in to opts.org, else to the stored sign-in's organization, asking
// first when its user belongs to several; a browser sign-in chose one on the page. kept
// is how many organizations the current one was kept among without asking.
func (app *App) setupSignIn(cmd *cobra.Command, cfg *config.Config, opts signInOptions, ask orgAsker) (_ *config.Config, kept int, _ error) {
	res, err := app.signIn(cmd, cfg, opts)
	if err != nil {
		return nil, 0, err
	}
	if res.reused && opts.org.empty() {
		client, err := api.New(cfg, api.Options{Version: app.version, Credential: res.cred})
		if err != nil {
			return nil, 0, err
		}
		orgs, err := fetchOrganizations(cmd.Context(), client)
		if err != nil {
			return nil, 0, fmt.Errorf("list your organizations: %w", err)
		}
		switch {
		case len(orgs) < 2:
		case ask == nil:
			kept = len(orgs)
		default:
			org, err := ask(orgs, res.cred.OrganizationID)
			if err != nil {
				return nil, 0, err
			}
			if org.ID != res.cred.OrganizationID {
				opts.org = orgRef{ID: org.ID, Name: org.Name}
				if _, err := app.signIn(cmd, cfg, opts); err != nil {
					return nil, 0, err
				}
			}
		}
	}
	cfg, err = app.loadConfig()
	return cfg, kept, err
}

// askOrganization is setup's orgAsker: a picker on a terminal a person watches, else nil.
func askOrganization(cmd *cobra.Command, assumeYes bool) orgAsker {
	if assumeYes || !canPrompt() {
		return nil
	}
	return func(orgs []organization, current string) (*organization, error) {
		return pickOrganization(cmd, orgs, current)
	}
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

	if opts.pauseBeforeBrowser && !opts.noBrowser && canPrompt() {
		if err := waitForBrowserEnter(cmd); err != nil {
			return nil, err
		}
	}

	client := api.NewAnonymous(cfg.AuthURL, app.version)
	cred, err := auth.Login(ctx, client, auth.LoginOptions{
		AppURL:       cfg.AppURL,
		Label:        auth.DefaultLabel(),
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

	replaced, err := auth.SaveCredential(app.dir, cfg.ProfileName, cred)
	if err != nil {
		return nil, err
	}
	// Best-effort: the superseded session may already be dead, often why the user is here.
	if replaced != nil {
		_ = app.revokeSession(ctx, cfg, replaced)
	}
	if err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) { applyLogin(p, cfg, cred, orgName) }); err != nil {
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
		cred, err = auth.LoadCredentialFor(app.dir, cfg.ProfileName, want.ID)
	case want.Name != "":
		return nil, false, nil
	default:
		cred, err = auth.LoadCredential(app.dir, cfg.ProfileName)
	}
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.RefreshToken == "" {
		return nil, false, nil
	}

	verified, identity, err := app.verifyCredential(ctx, cfg, cred)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		_ = auth.DeleteCredentialFor(app.dir, cfg.ProfileName, cred.OrganizationID)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// Saved as a sign-in would, so running setup again moves the tokens to where they now belong.
	if _, err := auth.SaveCredential(app.dir, cfg.ProfileName, verified); err != nil {
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
		_ = auth.UpdateCredential(app.dir, cfg.ProfileName, verified)
	}
	if err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) { applyLogin(p, cfg, verified, orgName) }); err != nil {
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
	creds, err := auth.Credentials(app.dir, cfg.ProfileName)
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

// applyLogin records only the account scope, and the environment the credential belongs
// to: hooks and the flushes they start run without TERMA_ENV, and would otherwise resolve
// to production and hold every event. Project selection belongs to repositories.
func applyLogin(p *config.Profile, cfg *config.Config, cred *auth.Credential, orgName string) {
	p.SelectOrganization(cred.OrganizationID, orgName)
	p.PinEnvironment(cfg.Environment)
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

type identityResponse struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
	AuthType       string `json:"auth_type"`
	UserID         string `json:"user_id"`
	Email          string `json:"email"`
}

// reportCredentialStore says where signing in kept the credentials, as gh does.
func reportCredentialStore(ui *setupUI, cfg *config.Config, insecure bool) {
	file := output.TildePath(config.CredentialsPath(cfg.Dir))
	switch {
	case !auth.StoredInFile(cfg.Dir, cfg.ProfileName):
		ui.Summary("Credentials", "in the system keychain")
	case insecure:
		ui.Summary("Credentials", "in plain text in "+file+" (--insecure-storage)")
	default:
		ui.Caution("Credentials", "in plain text in "+file+": no system keychain could be used")
	}
}

// settleSecrets keeps every secret with the sign-in: a sign-in that fell back to the file
// records that, so the relay's background writes go straight there rather than wait on a
// keychain setup could not use, and every other credential and key moves to where
// secrets now live.
func (app *App) settleSecrets(ui *setupUI, cfg *config.Config, insecure bool) {
	if !insecure && auth.StoredInFile(cfg.Dir, cfg.ProfileName) {
		if err := config.UpdateFile(cfg.Dir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
			fmt.Fprintf(ui.Detail(), "  could not record the plain-text fallback: %v\n", err)
		}
	}
	if err := auth.Relocate(cfg.Dir); err != nil {
		fmt.Fprintf(ui.Detail(), "  could not move the other organizations' credentials: %v\n", err)
	}
	if err := keystore.Relocate(cfg.Dir); err != nil {
		fmt.Fprintf(ui.Detail(), "  could not move the team keys: %v\n", err)
	}
}

// signedInAs is who the sign-in left this profile as: the email and organization.
func signedInAs(cfg *config.Config) string {
	who := "signed in"
	if cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName); err == nil && cred.UserEmail != "" {
		who = cred.UserEmail
	}
	if org := cmp.Or(cfg.OrganizationName, cfg.OrganizationID); org != "" {
		who += " (" + org + ")"
	}
	return who
}
