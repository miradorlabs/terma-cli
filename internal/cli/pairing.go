package cli

import (
	"errors"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// A profile's organization and team are paired with the sign-in they were chosen under
// (config.Profile.Account). Every writer of them compares that pairing and sets it in the
// one write to config.json; every reader takes the sign-in's lane only while it matches.

// pairedLane is lane, a person's sign-in as auth.Account names it, as the profile chosen
// under account (config.Profile.Account) pairs with it: the organization and team go only
// with the sign-in they were chosen under, so one stored as someone else, before the
// profile is rewritten for it (storeLogin), is unreadable, never routed to the team the
// last person chose. Signed out stays signed out.
func pairedLane(lane, account string) string {
	if auth.ValidAccountKey(lane) && lane != account {
		return sendas.AccountUnreadable
	}
	return lane
}

// pairProfile pairs the profile with account, a login's (applyLogin), only while account is
// the sign-in stored: a compare-and-set, so of two logins racing, or a reuse racing a login,
// neither stamps its account over the other's credential.
func (app *App) pairProfile(cfg *config.Config, account string) error {
	moved := false
	err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) {
		if moved = auth.Account(app.dir, cfg.ProfileName, cfg.AuthURL) != account; !moved {
			applyLogin(p, cfg, account)
		}
	})
	if err == nil && moved {
		return errSignedInSince
	}
	return err
}

// errSignedInSince is a sign-in as someone else, stored while setup chose an organization.
var errSignedInSince = errors.New("this machine signed in as someone else while setup ran: run `terma setup` again")

// recordOrganization records org as the profile's, chosen under cred's sign-in, only while
// the profile is still paired with that sign-in (config.Profile.Account): a compare-and-set,
// so a sign-in as someone else stored meanwhile is never given this choice.
func recordOrganization(dir, profile string, cred *auth.Credential, org *organization) error {
	moved := false
	err := config.UpdateProfile(dir, profile, func(p *config.Profile) {
		if moved = p.Account != cred.Account(); !moved {
			p.SelectOrganization(org.ID, org.Name)
		}
	})
	if err == nil && moved {
		return errSignedInSince
	}
	return err
}
