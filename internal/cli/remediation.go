package cli

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/remediation"
)

// remediationNotice is what the platform's active remediation campaign asks of this
// machine, for doctor and setup. cfg may be nil, a configuration that did not load: the
// auth host then comes from the flags and the environment alone, and there is no user
// session. Any failure to read the campaign is silent but for TERMA_DEBUG.
func (app *App) remediationNotice(ctx context.Context, cfg *config.Config, userSession bool) remediation.Notice {
	authURL := app.remediationAuthURL(cfg)
	if authURL == "" {
		return remediation.Notice{}
	}
	c, err := remediation.Fetch(ctx, authURL, app.version)
	if err != nil {
		if os.Getenv("TERMA_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "terma: debug: remediation campaign not read: %v\n", err)
		}
		return remediation.Notice{}
	}
	n := remediation.Evaluate(c, app.version, userSession)
	n.KeyInEnv = cfg != nil && cfg.APIKey != ""
	return n
}

// doctorRemediation is doctor's notice, judged on the sign-in the profile has now. Only
// doctor reads it: `terma status` stays off the network.
func (app *App) doctorRemediation(ctx context.Context, cfg *config.Config) remediation.Notice {
	return app.remediationNotice(ctx, cfg, hasUserSession(cfg))
}

// remediationAuthURL is cfg's auth host, or without a configuration, the one the flags and
// the environment name; "" for an unknown environment.
func (app *App) remediationAuthURL(cfg *config.Config) string {
	if cfg != nil {
		return cfg.AuthURL
	}
	if u := cmp.Or(app.flags.authURL, os.Getenv("TERMA_AUTH_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	e, err := config.EndpointsFor(cmp.Or(app.flags.env, os.Getenv("TERMA_ENV")))
	if err != nil {
		return ""
	}
	return e.AuthURL
}

// hasUserSession reports whether cfg's profile is signed in as a person, for this
// environment, with a sign-in that still sends: not a server key, nor none, nor a
// retired one.
func hasUserSession(cfg *config.Config) bool {
	if cfg == nil || cfg.APIKey != "" || cfg.ServerKeySignIn {
		return false
	}
	ident, err := auth.LoadIdentity(cfg.Dir, cfg.ProfileName)
	return err == nil && ident.CheckEnvironment(cfg.AuthURL) == nil && ident.RetiredAt.IsZero() &&
		auth.AccountKey(ident.AuthURL, ident.UserID) != ""
}

// cautionRemediation prints, before setup asks anything, what the platform's active
// remediation campaign asks of this machine; setup then carries on. The sign-in judged is
// the one setup leaves: a server key's under TERMA_API_KEY, else the person's.
func (app *App) cautionRemediation(ctx context.Context, ui *setupUI, cfg *config.Config) {
	n := app.remediationNotice(ctx, cfg, cfg.APIKey == "")
	if !n.Warn() {
		return
	}
	ui.Caution("Required", n.Summary())
	for _, line := range n.Lines() {
		ui.keep(" ", "", line)
	}
	ui.printLines()
}
