package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/setup"
)

// serverKeyAuth is whoami's auth_type for a server key.
const serverKeyAuth = "server_key"

// useServerKey has s sign in with the team server key in TERMA_API_KEY rather than as a
// person: the auth host names the key's organization and team, and once the key has
// fetched the team's policy it is kept as the team's key and the profile records them, so
// nothing needs TERMA_API_KEY again. Nothing is minted and no credentials.json written.
// org is --org and insecure --insecure-storage; a setup that fails before the key is kept
// leaves the keystore, the secret storage and the profile's sign-in as they were.
func (app *App) useServerKey(s *setup.Steps, ui *setupUI, org orgRef, team *string, insecure bool) {
	s.SignIn = func(ctx context.Context, cfg *config.Config) (*config.Config, error) {
		perms, err := app.serverKeySignIn(ctx, cfg, org)
		if err != nil {
			return nil, err
		}
		ui.Summary("Signed in", "server key "+keystore.Mask(cfg.APIKey)+" ("+cmp.Or(cfg.OrganizationName, cfg.OrganizationID)+")")
		var also []string
		if perms.Read {
			also = append(also, "read")
		}
		if perms.Write {
			also = append(also, "write")
		}
		if len(also) > 0 {
			ui.Caution("Server key", "it can also "+strings.Join(also, " and ")+
				" the team's data, and anything on this machine can use it: an Ingest-only key is enough")
		}
		return cfg, nil
	}
	s.SelectTeam = func(_ context.Context, cfg *config.Config) error {
		cfg.ProjectID, *team = cfg.Team, cfg.Team
		return nil
	}
	s.FetchPolicy = func(ctx context.Context, cfg *config.Config) (config.Policy, error) {
		pol, err := app.policies().FetchWithKey(ctx, cfg, cfg.APIKey)
		// The sign-in is kept here, once the key has proved it reads the policy and before
		// setup stores that policy, so a failed fetch writes nothing.
		if err == nil {
			err = app.keepServerKey(cfg, insecure)
		}
		if err != nil {
			return config.Policy{}, err
		}
		return pol, nil
	}
	s.SpoolKey = func(_ context.Context, cfg *config.Config) {
		ui.OK("Hook events", "delivered with the server key "+keystore.Mask(cfg.APIKey))
	}
}

// serverKeySignIn records the organization and team of cfg's server key in cfg, marked as
// signed in with it, and returns what the key may do. It refuses a credential that is no
// team server key, a key that cannot ingest, and an --org or --team the key does not
// belong to; it writes nothing.
func (app *App) serverKeySignIn(ctx context.Context, cfg *config.Config, org orgRef) (keyPermissions, error) {
	client, err := app.newClient(cfg)
	if err != nil {
		return keyPermissions{}, err
	}
	var id identityResponse
	if err := client.AuthGet(ctx, "/v1/whoami", nil, &id); err != nil {
		return keyPermissions{}, fmt.Errorf("check the server key in TERMA_API_KEY: %w", err)
	}
	known := ""
	if cfg.OrganizationID == id.OrganizationID {
		known = cfg.OrganizationName
	}
	switch {
	case id.AuthType != serverKeyAuth || !serverkey.Is(cfg.APIKey):
		return keyPermissions{}, errors.New("TERMA_API_KEY is set, but not to a team server key (" + serverkey.Display + "…) — set one, or unset it to sign in as a person")
	case id.OrganizationID == "" || id.ProjectID == "":
		return keyPermissions{}, errors.New("the server key in TERMA_API_KEY belongs to no team — use a key minted for one team")
	case id.Permissions == nil || !id.Permissions.Ingest:
		return keyPermissions{}, errors.New("the server key in TERMA_API_KEY cannot send telemetry — use a key with the Ingest permission")
	case org.Name != "":
		return keyPermissions{}, fmt.Errorf("--org %s: under a server key, setup cannot look up an organization by name — pass its id, or leave --org out", org.Name)
	case org.ID != "" && org.ID != id.OrganizationID:
		return keyPermissions{}, fmt.Errorf("TERMA_API_KEY is set — its server key signs in to its own organization; unset it to sign in to %s as a person", org)
	case cfg.ProjectID != "" && !uuidPattern.MatchString(cfg.ProjectID):
		return keyPermissions{}, fmt.Errorf("--team %s: under a server key, setup cannot look up a team by name — pass its id, or leave --team out", cfg.ProjectID)
	case cfg.ProjectID != "" && cfg.ProjectID != id.ProjectID:
		return keyPermissions{}, fmt.Errorf("--team %s: the server key in TERMA_API_KEY is team %s's, the only team setup can set up with it — leave --team out", cfg.ProjectID, id.ProjectID)
	}
	cfg.OrganizationID, cfg.OrganizationName = id.OrganizationID, known
	cfg.Team, cfg.ServerKeySignIn = id.ProjectID, true
	cfg.ProfileEnvironment = cfg.Environment
	return *id.Permissions, nil
}

// keepServerKey records insecure as where secrets are kept, stores cfg's server key there as
// its team's key, in place of every agent's own key for the team, which the relay would
// otherwise go on exporting with, and then records the key's organization and team on
// cfg's profile, marked as signed in with it. A key it cannot store leaves the secret
// storage as it was.
func (app *App) keepServerKey(cfg *config.Config, insecure bool) error {
	was := config.InsecureStorage(app.dir)
	if err := config.UpdateFile(app.dir, func(file *config.File) { file.InsecureStorage = insecure }); err != nil {
		return err
	}
	if err := keystore.Set(app.dir, cfg.Team, cfg.APIKey, keystore.HostsOf(cfg)); err != nil {
		_ = config.UpdateFile(app.dir, func(file *config.File) { file.InsecureStorage = was })
		return fmt.Errorf("store the server key: %w", err)
	}
	if err := keystore.DeleteHarnessKeys(app.dir, cfg.Team); err != nil {
		return fmt.Errorf("store the server key: %w", err)
	}
	// A key an earlier setup kept elsewhere stays there on Set, and so does a login kept
	// behind the key: move every secret to where secrets are now kept, as a browser sign-in
	// does (settleSecrets).
	if err := auth.Relocate(app.dir); err != nil {
		return fmt.Errorf("move the sign-in credentials to where secrets are kept: %w", err)
	}
	if err := keystore.Relocate(app.dir); err != nil {
		return fmt.Errorf("move the team keys to where secrets are kept: %w", err)
	}
	return config.UpdateProfile(app.dir, cfg.ProfileName, func(p *config.Profile) { applyServerKey(p, cfg) })
}

// applyServerKey records a server key's organization and team, and the environment it
// belongs to, as applyLogin does for a login, and marks the profile as signed in with it.
func applyServerKey(p *config.Profile, cfg *config.Config) {
	p.SelectOrganization(cfg.OrganizationID, "")
	p.PinEnvironment(cfg.Environment)
	p.Team = cfg.Team
	p.ServerKeySignIn = true
}
