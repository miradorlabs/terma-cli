package cli

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
)

// migrateAutomatically moves every machine to routing by teams at its relay's next start,
// whether or not its developer opted in. Off, a machine moves only once its developer
// opts in (migrate.OptIn): `terma teams`, or `terma setup` on a machine never set up
// before. A product decision: flipping it is the whole change.
const migrateAutomatically = false

// relayMigration is what the relay migrates at start (daemon.Config.Migration): this
// machine's directories, the relay's own terma, exe ("" for this process's), the termas
// the agents' machine-wide hooks run, and each team's organization type asked with its
// profile's own sign-in. A relay start migrates a machine not yet migrated once its
// developer opted in (migrateAutomatically), and finishes a migration an earlier start
// left either way (internal/migrate).
func (app *App) relayMigration(exe string) *migrate.Machine {
	return &migrate.Machine{ConfigDir: app.dir, StateDir: app.stateDir, Executable: exe,
		Hooked: app.hookedTermas, OrgType: app.organizationType, Automatic: migrateAutomatically}
}

// optedIn reports whether a relay start moves this machine to routing by teams: its
// developer opted in, or every machine moves (migrateAutomatically).
func (app *App) optedIn() bool {
	return migrateAutomatically || migrate.OptedIn(app.stateDir)
}

// hookedTermas lists, by path, the termas every agent's machine-wide hook entries run, read
// from each agent's hooks file (hookmgr.HookedTerma). A hooks file that cannot be read is
// an error: the migration then does not know which terma the hooks run, and waits.
func (app *App) hookedTermas() ([]string, error) {
	var out []string
	for _, a := range app.agents.With[agents.UserHooks]() {
		path, err := a.UserHooksPath()
		if err != nil {
			return nil, fmt.Errorf("%s's hooks file: %w", a.DisplayName(), err)
		}
		termas, err := hookmgr.HookedTerma(path)
		if err != nil {
			return nil, fmt.Errorf("%s's hooks: %w", a.DisplayName(), err)
		}
		out = append(out, termas...)
	}
	return out, nil
}

// organizationType is organizationID's type, "personal" or "shared", as profile's own
// sign-in to it lists it (GET /v1/organizations), in the environment profile resolves to
// as the migration reads it. A sign-in of another environment, or one that is refused,
// is an error: the migration then waits for a start that can ask.
func (app *App) organizationType(ctx context.Context, profile, organizationID string) (string, error) {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{Profile: profile})
	if err != nil {
		return "", err
	}
	cred, err := auth.LoadCredentialFor(app.dir, profile, organizationID)
	if err != nil {
		return "", err
	}
	if err := cred.CheckEnvironment(cfg.AuthURL); err != nil {
		return "", err
	}
	org, err := app.lookupOrganization(ctx, cfg, cred, organizationID)
	if err != nil {
		return "", err
	}
	return org.Type, nil
}
