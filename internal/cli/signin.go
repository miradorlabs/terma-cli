package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
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

func (r orgRef) String() string { return cmp.Or(r.Name, r.ID) }

type signInOptions struct {
	// org is --org: the organization to narrow setup to; empty discovers all organizations.
	org                orgRef
	noBrowser          bool
	pauseBeforeBrowser bool
}

// signIn is the one way a command obtains a credential: the stored sign-in verified
// against the auth host, else the browser.
func (app *App) signIn(cmd *cobra.Command, cfg *config.Config, opts signInOptions) (*auth.Credential, error) {
	if cfg.APIKey != "" {
		return nil, errors.New("TERMA_API_KEY is set — unset it to sign in as a user, or keep using the server key")
	}
	ctx := cmd.Context()
	if cred, ok, err := app.reuseStoredSession(ctx, cfg, nil); err != nil {
		return nil, err
	} else if ok {
		return cred, nil
	}

	if opts.pauseBeforeBrowser && !opts.noBrowser && canPrompt() {
		if err := waitForBrowserEnter(cmd); err != nil {
			return nil, err
		}
	}
	installation, err := config.InstallationID(app.dir)
	if err != nil {
		return nil, err
	}
	client := api.NewSignIn(cfg.AuthURL, app.version, installation)
	cred, err := auth.Login(ctx, client, auth.LoginOptions{
		AppURL:    cfg.AppURL,
		Label:     auth.DefaultLabel(),
		NoBrowser: opts.noBrowser,
		Out:       cmd.ErrOrStderr(),
	})
	if err != nil {
		return nil, err
	}
	if err := app.storeLogin(ctx, cfg, cred); err != nil {
		return nil, err
	}
	return cred, nil
}

// storeLogin keeps a new sign-in as the profile's, then ends the session it replaced: a
// machine keeps one, and storing first means a crash in between leaves the old one live
// but unused, never the machine signed out. A sign-in that cannot be stored is ended too,
// so it does not linger on the web. An older terma's sign-ins and the team keys it minted
// go with it. Another person's sign-in leaves no organization or team chosen behind: the
// profile is paired with it (applyLogin) before the old session's revoke is waited on, and
// until then the profile is still the other account's, which every reader refuses.
func (app *App) storeLogin(ctx context.Context, cfg *config.Config, cred *auth.Credential) error {
	replaced, err := auth.SaveCredential(app.dir, cfg.ProfileName, cred)
	if err != nil {
		_ = app.revokeSession(ctx, cfg, cred)
		return err
	}
	err = app.pairProfile(cfg, cred.Account())
	// Best-effort: the superseded session may already be dead, often why the user is here.
	// No process presents its token any more (a refresh rotates only the stored session).
	if replaced != nil {
		_ = app.revokeSession(ctx, cfg, replaced)
	}
	if err != nil {
		return err
	}
	// After applyLogin: a profile that leaves its server key for a sign-in keeps it no more.
	app.retireOlderSignIns(ctx, cfg.AuthURL)
	return nil
}

// retireOlderSignIns ends and deletes what an older terma signed this machine in with:
// its per-organization sessions, revoked best-effort with their own tokens, and the team
// keys it minted with them. A server key a profile was set up with (TERMA_API_KEY) stays.
func (app *App) retireOlderSignIns(ctx context.Context, authURL string) {
	app.retireLegacySessions(ctx, authURL)
	_ = keystore.RetainOnly(app.dir, serverKeyTeams(app.dir))
}

// retireLegacySessions revokes, best-effort, and deletes the per-organization sessions an
// older terma signed in: all at once, within one deadline, so an unreachable auth host
// delays a sign-in by seconds, not seconds per organization. A token goes only to authURL,
// the auth host this profile is configured for: credentials.json names hosts, and a
// tampered one must not lead a keychain-held token elsewhere; another environment's
// sessions are forgotten unrevoked, to expire.
func (app *App) retireLegacySessions(ctx context.Context, authURL string) {
	if signIns, err := auth.LegacySignIns(app.dir, true); err == nil {
		revokeCtx, cancel := context.WithTimeout(ctx, legacyRevokeWait)
		var wg sync.WaitGroup
		for _, s := range signIns {
			if s.AccessToken != "" && sameHost(s.AuthURL, authURL) {
				wg.Go(func() { _ = api.RevokeLegacySession(revokeCtx, s.AuthURL, app.version, s.AccessToken) })
			}
		}
		wg.Wait()
		cancel()
	}
	_ = auth.ForgetLegacy(app.dir)
}

// legacyRevokeWait bounds the best-effort revokes of an older terma's sessions.
const legacyRevokeWait = 5 * time.Second

// serverKeyTeams are the teams whose server key a profile signed in with.
func serverKeyTeams(dir string) []string {
	file, err := config.LoadFile(dir)
	if err != nil {
		return nil
	}
	var teams []string
	for _, p := range file.Profiles {
		if p != nil && p.ServerKeySignIn && p.Team != "" {
			teams = append(teams, p.Team)
		}
	}
	return teams
}

// sessionCheck decides, once the auth host has verified a stored credential, whether it
// may be reused, before anything is recorded; whoami is the host's answer about it. An
// error refuses the sign-in and stops setup.
type sessionCheck func(verified *auth.Credential, whoami identityResponse) (bool, error)

// errKeychainLocked is a stored sign-in the system keychain will not give up now: a new one
// could not replace it, so setup stops before a browser sign-in or a grant is spent.
var errKeychainLocked = errors.New("the system keychain holding your sign-in is locked or unavailable: unlock it (on macOS over SSH, `security unlock-keychain`) and run `terma setup` again, or pass --insecure-storage to keep credentials in a file")

// reuseStoredSession returns the profile's stored sign-in once the auth host has verified
// it, and accept, when not nil, takes it; ok false means open the browser. A sign-in the
// host refuses is forgotten, so it is not tried again; one accept refuses stays stored.
func (app *App) reuseStoredSession(ctx context.Context, cfg *config.Config, accept sessionCheck) (*auth.Credential, bool, error) {
	cred, err := auth.LoadCredential(app.dir, cfg.ProfileName)
	if secret.IsUnavailable(err) && !config.InsecureStorage(app.dir) {
		// Its tokens can't be read, so a new sign-in kept in the keychain could not replace
		// them (saveReplacing): stop before the browser, or a grant, is spent on one.
		return nil, false, errKeychainLocked
	}
	if err != nil || cred.CheckEnvironment(cfg.AuthURL) != nil || cred.RefreshToken == "" {
		return nil, false, nil
	}
	verified, identity, err := app.verifyCredential(ctx, cfg, cred)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		_ = auth.DeleteCredentialIf(app.dir, cfg.ProfileName, cred.RefreshToken)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if accept != nil {
		if ok, err := accept(verified, identity); err != nil || !ok {
			return nil, false, err
		}
	}
	// Kept where a sign-in would keep it now, so running setup again moves the tokens.
	if err := auth.Relocate(app.dir); err != nil {
		return nil, false, err
	}
	// Another process may have rotated the pair since it was read: take the stored one. One
	// that signed in again since replaced this session, which is over: never set up with it.
	switch stored, err := auth.LoadCredential(app.dir, cfg.ProfileName); {
	case err != nil:
		return nil, false, err
	case stored.SessionID != verified.SessionID:
		return nil, false, errors.New("this machine signed in again while setup ran: run `terma setup` again")
	default:
		verified = stored
	}
	verified.UserEmail = cmp.Or(identity.Email, verified.UserEmail)
	if err := app.pairProfile(cfg, verified.Account()); err != nil {
		return nil, false, err
	}
	// A first retirement the keychain or the network cut short is tried again.
	if auth.HasLegacySignIns(app.dir) {
		app.retireOlderSignIns(ctx, cfg.AuthURL)
	}
	return verified, true, nil
}

// verifyCredential returns the credential as it now stands: possibly a rotated pair,
// already stored.
func (app *App) verifyCredential(ctx context.Context, cfg *config.Config, cred *auth.Credential) (*auth.Credential, identityResponse, error) {
	client, err := api.New(cfg, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return nil, identityResponse{}, err
	}
	var identity identityResponse
	if err := client.AuthGet(ctx, "/v1/whoami", nil, &identity); err != nil {
		return nil, identityResponse{}, err
	}
	return client.Credential(), identity, nil
}

// errNoOrganization is a sign-in of someone who belongs to no organization yet.
var errNoOrganization = errors.New("you belong to no Terma organization yet: create or join one in the Terma app, then run `terma setup` again")

// errNotYourOrganization is an --org the person's organizations, as the account service
// lists them, do not include.
var errNotYourOrganization = errors.New("not one of your organizations")

func fetchOrganizations(ctx context.Context, client *api.Client) ([]organization, error) {
	var resp listOrganizationsResponse
	if err := client.AuthGet(ctx, "/v1/organizations", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Organizations, nil
}

func matchOrganization(orgs []organization, query string) (*organization, error) {
	return organizationKind.match(orgs, query)
}

// errRefreshInDoubt is a session left unrevoked because its refresh token may be spent.
var errRefreshInDoubt = errors.New("a refresh of this session got no answer, so its refresh token may be spent; it is left to expire")

// revokeSession ends cred's session at the auth host this profile is configured for, the
// one that issued it: best-effort, and the caller forgets cred whatever the answer. A token
// a refresh may have spent is never sent: presenting a spent token is reuse, which revokes
// the whole family. Nor is one to any other host: user-credentials.json names its host,
// and a tampered one must not lead a keychain-held token elsewhere, so another
// environment's session is left to expire.
func (app *App) revokeSession(ctx context.Context, cfg *config.Config, cred *auth.Credential) error {
	if cred.RefreshToken == "" {
		return nil
	}
	if !cred.RefreshInDoubtSince.IsZero() {
		return errRefreshInDoubt
	}
	if err := cred.CheckEnvironment(cfg.AuthURL); err != nil {
		return fmt.Errorf("the session was not revoked, and is left to expire: %w", err)
	}
	_, err := api.RevokeRefreshToken(ctx, cfg.AuthURL, app.version, cred.RefreshToken)
	return err
}

// applyLogin records the environment the credential belongs to: hooks and the flushes they
// start run without TERMA_ENV, and would otherwise resolve to production and hold every
// event. The login is the profile's credential from then on, not a server key setup stored.
// It pairs the profile with account, the login's: an organization and team chosen under any
// other account, or under none, are cleared in the same write.
func applyLogin(p *config.Profile, cfg *config.Config, account string) {
	if p.Account != account {
		p.OrganizationID, p.OrganizationName, p.Team, p.TeamName = "", "", "", ""
	}
	p.Account = account
	p.PinEnvironment(cfg.Environment)
	p.ServerKeySignIn, p.ServerKeyAuthURL = false, ""
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
	// Permissions are a server key's; nil for a user token.
	Permissions *keyPermissions `json:"permissions,omitempty"`
}

// keyPermissions are what a server key may do with its team's data.
type keyPermissions struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Ingest bool `json:"ingest"`
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
		fmt.Fprintf(ui.Detail(), "  could not move the other profiles' credentials: %v\n", err)
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

// signedInSummary formats the Signed in summary line: email plus organization when scoped
// to a single organization, or email alone when setup covers multiple organizations.
func signedInSummary(cfg *config.Config, disc *setupDiscovery) string {
	if disc == nil {
		return signedInAs(cfg)
	}
	who := "signed in"
	if cred, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName); err == nil && cred.UserEmail != "" {
		who = cred.UserEmail
	}
	if len(disc.orgs) == 1 {
		if org := cmp.Or(disc.orgs[0].Name, disc.orgs[0].ID); org != "" {
			who += " (" + org + ")"
		}
	}
	return who
}
