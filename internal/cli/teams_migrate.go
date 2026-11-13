package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// defaultMigrationWait bounds how long `terma teams` waits for the relay it started to
// migrate the machine: a move of keys and sign-ins, done once.
const defaultMigrationWait = 30 * time.Second

// errMigrationIncomplete is `terma teams` giving up on a migration it started: the machine
// is as it was, collecting by its one team.
var errMigrationIncomplete = errors.New("this machine's move to routing by teams did not finish, so nothing was changed: run `terma doctor` to see why")

// migrateFirst moves a machine not yet migrated to routing by teams before `terma teams`
// changes anything: only a relay migrates, at start, holding the relay's and the spool
// flush's locks. So it stops the running relay, which drops what it held in memory, starts
// this executable's, and waits for the migration to commit for profile. It fails without
// changing anything when the commit does not come.
func (app *App) migrateFirst(cmd *cobra.Command, profile string) error {
	if config.Migrated(app.dir) && migrationCommitted(app.stateDir, profile) {
		return nil
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Moving this machine to routing by teams: restarting the relay…")
	// A migration a crash left in its preparing phase is finished by the next relay start too.
	dir, err := daemon.Dir(app.stateDir)
	if err != nil {
		return err
	}
	daemon.Stop(dir)
	exe, err := app.hookExecutable()
	if err != nil {
		return err
	}
	app.spawnRelay(exe, app.stateDir, app.version)
	wait := app.migrationWait
	if wait == 0 {
		wait = defaultMigrationWait
	}
	ctx := cmd.Context()
	for deadline := time.Now().Add(wait); ; {
		if config.Migrated(app.dir) && migrationCommitted(app.stateDir, profile) {
			return nil
		}
		if time.Now().After(deadline) {
			return errMigrationIncomplete
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// migrationCommitted reports whether profile's routing state is past the migration: not
// in its preparing phase, which collects nothing until the relay commits it. A profile the
// migration made no state for counts too: `terma teams` then says to run setup.
func migrationCommitted(stateDir, profile string) bool {
	st, ok, err := routing.Load(stateDir, profile)
	if err != nil {
		// Unreadable is past the migration: nothing waiting will mend it.
		return true
	}
	return !ok || st.Migration == nil || st.Migration.Phase == routing.Committed
}
