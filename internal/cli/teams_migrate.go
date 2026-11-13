package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// defaultMigrationWait bounds how long `terma teams` waits for the relay it started to
// migrate the machine: a move of keys and sign-ins, done once.
const defaultMigrationWait = 30 * time.Second

// errMigrationIncomplete is `terma teams` giving up on a migration it started: the machine
// is as it was, collecting by its one team.
var errMigrationIncomplete = errors.New("this machine's move to routing by teams did not finish, so nothing was changed")

// migrateFirst moves a machine not yet migrated to routing by teams before `terma teams`
// changes anything: only a relay migrates, at start, holding the relay's and the spool
// flush's locks. So it stops the running relay, which drops what it held in memory, starts
// this executable's, and waits until the machine has no migrating left to do
// (migrate.Needed). It fails without changing anything when that does not come, saying
// why when the relay recorded it (migrate.ReadBlock), as soon as it does.
func (app *App) migrateFirst(cmd *cobra.Command, profile string) error {
	if !migrate.Needed(app.dir, app.stateDir) {
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
	started := time.Now().UTC()
	app.spawnRelay(exe, app.stateDir, app.version)
	wait := app.migrationWait
	if wait == 0 {
		wait = defaultMigrationWait
	}
	ctx := cmd.Context()
	for deadline := time.Now().Add(wait); ; {
		if !migrate.Needed(app.dir, app.stateDir) {
			return nil
		}
		// A record from before this start says why an earlier one did not migrate.
		if b, ok := migrate.ReadBlock(app.stateDir); ok && !b.At.Before(started.Truncate(time.Second)) {
			return migrationBlockedError(b)
		}
		if time.Now().After(deadline) {
			if b, ok := migrate.ReadBlock(app.stateDir); ok {
				return migrationBlockedError(b)
			}
			return fmt.Errorf("%w: run `terma doctor` to see why", errMigrationIncomplete)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// migrationBlockedError is errMigrationIncomplete, with why the relay did not migrate
// and what to do about it.
func migrationBlockedError(b migrate.Block) error {
	w := blockWords(b)
	return fmt.Errorf("%w: %s; %s", errMigrationIncomplete, w.Detail, w.Fix)
}
