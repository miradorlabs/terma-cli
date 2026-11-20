package cli

import (
	"cmp"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

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
