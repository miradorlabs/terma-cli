package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// browserSignIn is one approval in the browser, which signs in to every organization
// ticked there (contract C7): want is preselected and orgs pre-checked, every organization
// when orgs is empty. Each sign-in is saved without activating it, and the session it
// displaces is ended. Then exactly one organization is activated (activeAfterSignIn) and
// the profile records it, once. It says which organizations signed in, and names each one
// the developer belongs to that this machine still has no sign-in for.
func (app *App) browserSignIn(cmd *cobra.Command, cfg *config.Config, want orgRef, orgs []string, noBrowser bool) (*signInResult, error) {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	results, _, err := auth.LoginAll(ctx, api.NewAnonymous(cfg.AuthURL, app.version), auth.LoginOptions{
		AppURL:        cfg.AppURL,
		Label:         auth.DefaultLabel(),
		Organization:  want.hint(),
		Organizations: orgs,
		NoBrowser:     noBrowser,
		Out:           errOut,
	})
	if err != nil {
		return nil, err
	}
	saved, err := app.storeLogins(ctx, cfg, results, errOut)
	if err != nil {
		return nil, err
	}

	// The listing types the personal organization and names those still without a
	// sign-in; any sign-in just obtained reads it. Without it neither is known.
	var listed []organization
	for _, r := range saved {
		if client, err := api.New(cfg, api.Options{Version: app.version, Credential: r.Credential}); err == nil {
			if listed, err = fetchOrganizations(ctx, client); err == nil {
				break
			}
		}
	}
	var asked, personal string
	var signedIn, listedIDs []string
	for _, r := range saved {
		signedIn = append(signedIn, r.Credential.OrganizationID)
		if !want.empty() && want.matches(r.Credential.OrganizationID, r.OrganizationName) {
			asked = r.Credential.OrganizationID
		}
	}
	for _, o := range listed {
		listedIDs = append(listedIDs, o.ID)
		if o.Type == "personal" && personal == "" {
			personal = o.ID
		}
	}
	has := func(id string) bool {
		_, ok := app.orgSignIn(cfg, id)
		return slices.Contains(signedIn, id) || ok
	}
	active := activeAfterSignIn(asked, cfg.OrganizationID, personal, listedIDs, signedIn, has)

	res := &signInResult{}
	if i := slices.Index(signedIn, active); i >= 0 {
		res.cred, res.orgName = saved[i].Credential, saved[i].OrganizationName
	} else if res.cred, err = auth.LoadCredentialFor(app.dir, cfg.ProfileName, active); err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(listed, func(o organization) bool { return o.ID == active }); res.orgName == "" && i >= 0 {
		res.orgName = listed[i].Name
	}
	if res.orgName == "" && cfg.OrganizationID == active {
		res.orgName = cfg.OrganizationName
	}
	if err := auth.ActivateCredential(app.dir, cfg.ProfileName, active); err != nil {
		return nil, err
	}
	if err := config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) { applyLogin(p, cfg, res.cred, res.orgName) }); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(saved))
	for _, r := range saved {
		names = append(names, loginName(r, listed))
	}
	fmt.Fprintf(errOut, "Signed in to %s\n", output.And(names))
	for _, o := range listed {
		if !has(o.ID) {
			fmt.Fprintf(errOut, "Not signed in to %s. Run `terma teams` to authorize it.\n", cmp.Or(o.Name, o.ID))
		}
	}
	return res, nil
}

// activeAfterSignIn is the one organization a sign-in leaves active (contract C7,
// invariant 6), whatever order its codes were exchanged in:
//
//  1. asked, the organization asked for, if it signed in now;
//  2. else current, the profile's organization before, if it still has a sign-in here;
//  3. else personal, the personal organization, if it has one;
//  4. else the first organization that signed in now, in the order listed lists them,
//     else in signedIn's order when they are not listed.
//
// signedIn are the organizations that signed in now, never empty; has reports whether
// this machine holds a sign-in for an organization, those included.
func activeAfterSignIn(asked, current, personal string, listed, signedIn []string, has func(string) bool) string {
	switch {
	case asked != "" && slices.Contains(signedIn, asked):
		return asked
	case current != "" && has(current):
		return current
	case personal != "" && has(personal):
		return personal
	}
	for _, id := range listed {
		if slices.Contains(signedIn, id) {
			return id
		}
	}
	return signedIn[0]
}

// loginName is how a sign-in's organization is named to the developer: as its exchange
// named it, else as listed, else its id.
func loginName(r auth.LoginResult, listed []organization) string {
	if r.OrganizationName != "" {
		return r.OrganizationName
	}
	if i := slices.IndexFunc(listed, func(o organization) bool { return o.ID == r.Credential.OrganizationID }); i >= 0 && listed[i].Name != "" {
		return listed[i].Name
	}
	return r.Credential.OrganizationID
}

// storeLogins keeps each new sign-in without activating it, replacing only its own
// organization's stored sign-in, which is then ended; the other organizations' stay
// stored. A sign-in that cannot be stored is ended too, so it does not linger on the web,
// and is said when others were stored; only when none is stored does it fail, with the
// first error.
func (app *App) storeLogins(ctx context.Context, cfg *config.Config, results []auth.LoginResult, errOut io.Writer) ([]auth.LoginResult, error) {
	var saved []auth.LoginResult
	var failed []string
	var firstErr error
	for _, r := range results {
		replaced, err := auth.SaveInactiveCredential(app.dir, cfg.ProfileName, r.Credential)
		if err != nil {
			_ = app.revokeSession(ctx, cfg, r.Credential)
			failed = append(failed, fmt.Sprintf("Could not store the sign-in to %s: %v", loginName(r, nil), err))
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		// Best-effort: the superseded session may already be dead, often why the user is here.
		if replaced != nil {
			_ = app.revokeSession(ctx, cfg, replaced)
		}
		saved = append(saved, r)
	}
	if len(saved) == 0 {
		return nil, firstErr
	}
	for _, line := range failed {
		fmt.Fprintln(errOut, line)
	}
	return saved, nil
}
