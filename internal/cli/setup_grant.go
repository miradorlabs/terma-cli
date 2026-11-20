package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// checkDesktopGrant refuses a `terma setup --desktop-grant` that could stop to ask: Terma
// Desktop names the organization and the team by id and answers nothing else. orgID is
// --org; the team is the root's --team.
func (app *App) checkDesktopGrant(f setupFlags) error {
	switch {
	case !f.assumeYes:
		return errors.New("--desktop-grant needs -y")
	case parseOrgRef(f.org).ID == "":
		return errors.New("--desktop-grant needs --org with the organization's id")
	case !uuidPattern.MatchString(app.flags.projectID):
		return errors.New("--desktop-grant needs --team with the team's id")
	case f.noBrowser:
		return errors.New("--desktop-grant opens no browser: drop --no-browser")
	case f.managedConfig != "":
		return errors.New("--desktop-grant signs this machine in: drop --managed-config")
	}
	return nil
}

// signInWithGrant is setup's sign-in under --desktop-grant: Terma Desktop writes who it is
// signed in as on stdin; this CLI's stored sign-in of that same user is reused, else the
// desktop delegates a user session of its own to this CLI's PKCE challenge
// (auth.LoginWithGrant). A sign-in of another user is never kept. Then the organization
// is --org, which must be one the user belongs to, and the desktop's when it names one.
func (app *App) signInWithGrant(cmd *cobra.Command, cfg *config.Config, orgID string) (*config.Config, error) {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintln(cmd.ErrOrStderr(), `terma setup --desktop-grant is for Terma Desktop: it reads {"user_id","org_id"} and then {"code"} as JSON lines on stdin.`)
	}
	desktop, err := auth.ReadGrantIdentity(ctx, in, 0)
	if err != nil {
		return nil, err
	}
	if desktop.OrgID != "" && !strings.EqualFold(desktop.OrgID, orgID) {
		return nil, &auth.GrantError{Code: auth.GrantBadInput, Message: "org_id does not match --org"}
	}
	// The user is the one the auth host answers for. Ids compare without regard to case,
	// as ReadGrantIdentity reads them.
	// The desktop's user's sign-in is reused only for an --org that user belongs to, checked
	// before the reuse records anything: a refused grant changes nothing.
	sameDesktop := func(verified *auth.Credential, whoami identityResponse) (bool, error) {
		if !strings.EqualFold(cmp.Or(whoami.UserID, verified.UserID), desktop.UserID) {
			return false, nil
		}
		client, err := api.New(cfg, api.Options{Version: app.version, Credential: verified})
		if err != nil {
			return false, err
		}
		orgs, err := fetchOrganizations(ctx, client)
		return err == nil, grantOrganizationError(orgs, err, orgID)
	}
	cred, reused, err := app.reuseStoredSession(ctx, cfg, sameDesktop)
	if err != nil {
		return nil, err
	}
	// A reused sign-in is announced only once --org is checked below, so stdout says
	// {"reused":true} or {"error":…}, never both.
	if !reused {
		installation, err := config.InstallationID(app.dir)
		if err != nil {
			return nil, err
		}
		client := api.NewSignIn(cfg.AuthURL, app.version, installation)
		cred, err = auth.LoginWithGrant(ctx, client, auth.GrantOptions{Label: auth.DesktopLabel(), In: in, Out: out})
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(cred.UserID, desktop.UserID) {
			// Never kept, and best-effort ended, so it does not linger on the web.
			_ = app.revokeSession(ctx, cfg, cred)
			return nil, &auth.GrantError{Code: auth.GrantIdentityMismatch,
				Message: "the code signed in another user than Terma Desktop's"}
		}
		cred.Label = auth.DesktopLabel()
		// --org is checked before the new sign-in replaces any stored one: a grant refused for
		// it keeps nothing, and ends what it made.
		// The pair as the check left it: a 401 there rotates it, and the one it spent must
		// never be stored or revoked.
		if cred, err = app.checkGrantOrganization(ctx, cfg, cred, orgID); err != nil {
			// Ended only when the check was answered: after a failure, a refresh it tried may
			// have left the token spent, and revoking a spent token ends the whole family.
			if _, refused := errors.AsType[*auth.GrantError](err); refused {
				_ = app.revokeSession(ctx, cfg, cred)
			}
			return nil, err
		}
		// Another user's stored sign-in is replaced and ended.
		if err := app.storeLogin(ctx, cfg, cred); err != nil {
			return nil, err
		}
	}
	if cfg, err = app.loadConfig(); err != nil {
		return nil, err
	}
	// The list reads the account service's own records, so an organization it leaves out is
	// not the user's, not merely unconfirmed. Any other failure is no bad input.
	switch _, err := app.chooseOrganization(ctx, cfg, cred, orgRef{ID: orgID}, nil); {
	case errors.Is(err, errNotYourOrganization):
		return nil, &auth.GrantError{Code: auth.GrantBadInput, Message: "--org is not an organization the signed-in user belongs to", Err: err}
	case errors.Is(err, errNoOrganization):
		return nil, &auth.GrantError{Code: auth.GrantBadInput, Message: "the signed-in user belongs to no Terma organization yet", Err: err}
	case err != nil:
		return nil, err
	}
	if reused {
		if err := auth.WriteGrantReused(out); err != nil {
			return nil, err
		}
	}
	return app.loadConfig()
}

// checkGrantOrganization refuses, as bad input, an --org the signed-in user does not belong
// to, judged with cred before it is stored. It returns the pair as the check left it, which
// a refresh on a 401 may have rotated: the one it spent is never to be used again.
func (app *App) checkGrantOrganization(ctx context.Context, cfg *config.Config, cred *auth.Credential, orgID string) (*auth.Credential, error) {
	unstored := *cfg
	unstored.ProfileName = "" // read with cred alone: nothing is stored or refreshed into the profile yet
	client, err := api.New(&unstored, api.Options{Version: app.version, Credential: cred})
	if err != nil {
		return cred, err
	}
	orgs, err := fetchOrganizations(ctx, client)
	return client.Credential(), grantOrganizationError(orgs, err, orgID)
}

// grantOrganizationError judges --org against the organizations the signed-in user's
// sign-in lists (or the error listing them): one they don't belong to, or none at all, is
// the desktop's bad input.
func grantOrganizationError(orgs []organization, err error, orgID string) error {
	switch {
	case err != nil:
		return err
	case len(orgs) == 0:
		return &auth.GrantError{Code: auth.GrantBadInput, Message: "the signed-in user belongs to no Terma organization yet"}
	}
	if _, err := matchOrganization(orgs, orgID); errors.As(err, new(noMatchError)) {
		return &auth.GrantError{Code: auth.GrantBadInput, Message: "--org is not an organization the signed-in user belongs to", Err: err}
	} else if err != nil {
		return err
	}
	return nil
}

// writeGrantFailure prints a failed grant as `terma auth token` prints its failures: the
// cause on stderr, {"error": {"code", "message"}} on stdout, and exit code 1.
func writeGrantFailure(stdout, stderr io.Writer, failure *auth.GrantError) error {
	fmt.Fprintf(stderr, "terma setup: %s: %v\n", failure.Code, failure)
	type grantFailure struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewEncoder(stdout).Encode(map[string]grantFailure{"error": {failure.Code, failure.Message}}); err != nil {
		return err
	}
	return exitWith(1)
}
