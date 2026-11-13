package cli

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
)

// defaultMigrationWait bounds how long `terma teams` waits for the relay it started to
// begin migrating the machine (migrate.Migrating): it takes its lock once the relay it
// replaced has stopped.
const defaultMigrationWait = 30 * time.Second

// migrationProgressWait bounds the wait once the relay has begun: more than it may take to
// ask each team's organization type, wait out a spool flush and move the keys and
// sign-ins. The relay records the outcome as it gets it, so this is only a backstop.
const migrationProgressWait = 5 * time.Minute

// errMigrationIncomplete is `terma teams` on a migration the relay did not make: the
// machine is as it was, collecting by its one team.
var errMigrationIncomplete = errors.New("this machine's move to routing by teams did not finish, so nothing was changed")

// errMigrationPartWay is `terma teams` on a migration the relay left part way: the machine
// collects nothing until a relay start finishes it.
var errMigrationPartWay = errors.New("this machine's move to routing by teams stopped part way, so it collects nothing until the relay finishes it")

// errMigrationPending is `terma teams` giving up its wait on a migration the relay may
// still make: whether it changes the machine is not known yet.
var errMigrationPending = errors.New("this machine's move to routing by teams has not finished yet: the relay goes on with it")

// errNotSetUp is `terma teams` on a machine `terma setup` never prepared: there is no relay
// to migrate it, and setup signs in and shows the same team checklist.
var errNotSetUp = errors.New("terma is not set up on this machine: run `terma setup`, which signs you in and shows this team checklist")

// migrateFirst moves a machine not yet migrated to routing by teams before `terma teams`
// changes anything: only a relay migrates, holding the relay's and the spool flush's
// locks. So it stops the running relay, which drops what it held in memory, starts this
// executable's, and waits on what that relay records (migrate.Migrating, migrate.ReadBlock)
// until the machine has no migrating left to do (migrate.Needed), the relay records why
// it did not migrate, or it stops without either. It fails saying so, and says nothing
// was changed only when that is known: past its bounds the move may still be made.
func (app *App) migrateFirst(cmd *cobra.Command, profile string) error {
	if !migrate.Needed(app.dir, app.stateDir) {
		return nil
	}
	// The relay refuses to start without the token setup writes, so waiting for it would
	// only time out.
	if _, err := daemon.Token(app.stateDir); err != nil {
		return errNotSetUp
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
	ctx := cmd.Context()
	begun := false
	for deadline := time.Now().Add(cmp.Or(app.migrationWait, defaultMigrationWait)); ; {
		// Read first: the relay records its outcome before it removes this record, so once
		// it has gone, the reads below see the outcome.
		since, migrating := migrate.Migrating(app.stateDir)
		if !migrate.Needed(app.dir, app.stateDir) {
			return nil
		}
		// A record from before this start says why an earlier one did not migrate.
		if b, ok := migrate.ReadBlock(app.stateDir); ok && !b.At.Before(started.Truncate(time.Second)) {
			return migrationBlockedError(b)
		}
		switch {
		case migrating && !since.Before(started):
			if !begun {
				begun, deadline = true, since.Add(migrationProgressWait)
			}
		case begun:
			// It ended with no outcome: the relay was stopped before it changed anything.
			return fmt.Errorf("%w: the relay stopped first; run `terma teams` again", errMigrationIncomplete)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w; run `terma doctor` to see where it stands", errMigrationPending)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// migrationBlockedError says why the relay did not migrate, or stopped part way, and what
// to do about it.
func migrationBlockedError(b migrate.Block) error {
	w := blockWords(b)
	if b.Unfinished {
		return fmt.Errorf("%w: %s; %s", errMigrationPartWay, w.Detail, w.Fix)
	}
	return fmt.Errorf("%w: %s; %s", errMigrationIncomplete, w.Detail, w.Fix)
}
