package cli

import (
	"cmp"
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
	sameDesktop := func(verified *auth.Credential, whoami identityResponse) bool {
		return strings.EqualFold(cmp.Or(whoami.UserID, verified.UserID), desktop.UserID)
	}
	cred, ok, err := app.reuseStoredSession(ctx, cfg, sameDesktop)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := auth.WriteGrantReused(out); err != nil {
			return nil, err
		}
	} else {
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
	if _, err := app.chooseOrganization(ctx, cfg, cred, orgRef{ID: orgID}, nil); errors.Is(err, errNotYourOrganization) {
		return nil, &auth.GrantError{Code: auth.GrantBadInput, Message: "--org is not an organization the signed-in user belongs to", Err: err}
	} else if err != nil {
		return nil, err
	}
	return app.loadConfig()
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
