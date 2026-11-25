package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// errServerKeyProfileMoved is a server-key profile that changed while its routing state
// was being written for it: nothing is written, and the next run reads it anew.
var errServerKeyProfileMoved = errors.New("the server-key profile changed while its routing state was written")

// migrateServerKeyRouting gives every server-key profile an earlier terma set up the
// routing state this one routes by, and returns the files it wrote. An earlier terma kept
// such a profile's team in config.json, its key in keys.json and its policy in
// policies/<team>.json, and collected from those alone; this one collects nothing without
// routing/<profile>.json, which only `terma setup` wrote, so a headless machine would stop
// collecting when it updated, with only `terma doctor` to say so.
//
// It runs where an update lands (refresh.Refresher: `terma update`, the relay's own
// update, the first command under a newer release) and as a relay of this build starts
// (writeServerKeyRouting), never from a hook: a hook only reads the routing state, and
// goes on failing closed without one. It reads what is on this machine and nothing else,
// so it signs in to nothing. Like setup's own first routing state, one is written only
// while no relay of an earlier build can be running (daemon.WithRoutingRelayGate), and only
// a machine with a profile to migrate takes that gate, which stops a running relay.
func (app *App) migrateServerKeyRouting(context.Context) ([]string, error) {
	names := app.serverKeyProfilesToMigrate()
	if len(names) == 0 {
		return nil, nil
	}
	var wrote []string
	err := daemon.WithRoutingRelayGate(app.stateDir, app.version, func() error {
		var err error
		wrote, err = app.writeServerKeyRouting(names)
		return err
	})
	return wrote, err
}

// serverKeyProfilesToMigrate are the profiles migrateServerKeyProfile would write a routing
// state for now: none on a machine that needs no migration, which is every one but a
// server-key machine on its first run of this build.
func (app *App) serverKeyProfilesToMigrate() []string {
	if app.stateDir == "" {
		return nil
	}
	file, err := config.LoadFile(app.dir)
	if err != nil {
		return nil
	}
	var names []string
	for name, p := range file.Profiles {
		if p == nil || !p.ServerKeySignIn {
			continue
		}
		if _, found, err := routing.Load(app.stateDir, name); found || err != nil {
			continue
		}
		if _, ok := app.serverKeyTeam(name); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// writeServerKeyRouting migrates each of the profiles names and returns the files it
// wrote. Its caller holds the relay's lock, or is the relay that holds it.
func (app *App) writeServerKeyRouting(names []string) ([]string, error) {
	var wrote []string
	var errs []error
	for _, name := range names {
		path, ok, err := app.migrateServerKeyProfile(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("profile %s: %w", name, err))
		} else if ok {
			wrote = append(wrote, path)
		}
	}
	return wrote, errors.Join(errs...)
}

// serverKeyTeam is the one team the server-key profile name collects for, with the policy
// an earlier terma validated for it, as setup's own first routing state would hold it
// (useServerKey): false unless the profile is signed in with a server key for the
// environment it is in, the key is on file, and the stored policy was fetched for that
// team, in that organization and environment. A key the keychain will not give up now is
// taken as not on file, and the next run asks again.
func (app *App) serverKeyTeam(name string) (routing.Team, bool) {
	cfg, err := app.loadProfileConfig(name)
	if err != nil || !cfg.ServerKeySignIn || cfg.Team == "" || cfg.OrganizationID == "" || cfg.AuthURL == "" || cfg.AuthURL != cfg.ServerKeyAuthURL {
		return routing.Team{}, false
	}
	pol, ok, err := config.ReadPolicy(app.stateDir, cfg.Team)
	if err != nil || !ok || pol.OrganizationID != cfg.OrganizationID || pol.AuthURL != cfg.AuthURL {
		return routing.Team{}, false
	}
	if key, err := keystore.Get(app.dir, cfg.Team); err != nil || !serverkey.Is(key) {
		return routing.Team{}, false
	}
	// Only a personal organization may collect everything, as setup holds a server key's team to.
	if pol.Global() {
		pol.Mode, pol.Repositories = config.ModeRepo, nil
	}
	return routing.Team{OrganizationID: cfg.OrganizationID, AuthURL: cfg.AuthURL, TeamID: cfg.Team, Policy: pol}, true
}

// migrateServerKeyProfile writes the one-team, no-person routing state of the server-key
// profile name when it has none, binding the repositories its policy lists to the team's
// organization as `TERMA_API_KEY=… terma setup` does unasked, and reports the file it
// wrote. A profile that is not such a profile, or already has a routing state, is left as
// it is: so is one whose state cannot be read, which stays failing closed until setup
// replaces it. The state is created under its lock only if there is still none
// (routing.Create), and only while the profile still names the same server-key team.
func (app *App) migrateServerKeyProfile(name string) (string, bool, error) {
	target, ok := app.serverKeyTeam(name)
	if !ok {
		return "", false, nil
	}
	if _, found, err := routing.Load(app.stateDir, name); found || err != nil {
		return "", false, nil
	}
	bindings := autoSetupBindings(target, nil)
	st, err := routing.Create(app.stateDir, name, sendas.NoPersonLane, func(s *routing.State) error {
		if now, ok := app.serverKeyTeam(name); !ok || now.TeamID != target.TeamID || now.OrganizationID != target.OrganizationID || now.AuthURL != target.AuthURL {
			return errServerKeyProfileMoved
		}
		if _, err := s.Select(target); err != nil {
			return err
		}
		return applySetupBindingsAll(s, bindings)
	})
	switch {
	case errors.Is(err, routing.ErrStateExists), errors.Is(err, errServerKeyProfileMoved):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	if err := app.persistCommittedPolicies(st); err != nil {
		return "", false, err
	}
	path, err := routing.StatePath(app.stateDir, name)
	return path, err == nil, err
}
