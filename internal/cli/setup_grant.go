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
// signed in as on stdin; a stored sign-in of this CLI's for that same user and organization
// is reused, else the desktop delegates one to this CLI's own PKCE challenge
// (auth.LoginWithGrant). A sign-in for another user or organization is never kept.
// orgID is --org, which the desktop's organization must be.
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
	if !strings.EqualFold(desktop.OrgID, orgID) {
		return nil, &auth.GrantError{Code: auth.GrantBadInput, Message: "org_id does not match --org"}
	}

	// The user is the one the auth host answers for: whoami fills it for a credential saved
	// before terma kept it.
	sameDesktop := func(verified *auth.Credential, whoami identityResponse) bool {
		return cmp.Or(whoami.UserID, verified.UserID) == desktop.UserID &&
			cmp.Or(whoami.OrganizationID, verified.OrganizationID) == desktop.OrgID && verified.OrganizationID == desktop.OrgID
	}
	if _, ok, err := app.reuseStoredSessionIf(ctx, cfg, orgRef{ID: desktop.OrgID}, sameDesktop); err != nil {
		return nil, err
	} else if ok {
		if err := auth.WriteGrantReused(out); err != nil {
			return nil, err
		}
		return app.loadConfig()
	}

	client := api.NewAnonymous(cfg.AuthURL, app.version)
	cred, err := auth.LoginWithGrant(ctx, client, auth.GrantOptions{Label: auth.DesktopLabel(), In: in, Out: out})
	if err != nil {
		return nil, err
	}
	if cred.UserID != desktop.UserID || cred.OrganizationID != desktop.OrgID {
		// Never kept, and best-effort ended, so it does not linger on the web.
		_ = app.revokeSession(ctx, cfg, cred)
		return nil, &auth.GrantError{Code: auth.GrantIdentityMismatch,
			Message: "the code signed in another user or organization than Terma Desktop's"}
	}
	cred.Label = auth.DesktopLabel()
	orgName := ""
	if result := client.LastLogin(); result != nil {
		orgName = result.OrganizationName
	}
	// Another user's stored sign-in for this organization is replaced and ended; the other
	// organizations' stay stored for a switch back.
	if err := app.storeLogin(ctx, cfg, cred, orgName); err != nil {
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
