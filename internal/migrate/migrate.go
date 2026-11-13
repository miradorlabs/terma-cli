// Package migrate is the machine's one-way move to multi-org routing (internal/routing).
// Only the relay runs it, once it has started listening under today's rules, holding the
// relay's lock and the spool flush's, so no other terma sends while it runs; the relay
// asks the config directory and the routing state as it routes each record, so it moves
// to the new rules as the migration commits. A machine moves only once its developer opts
// in (OptIn): `terma teams`, or `terma setup` on a machine never set up before. It covers
// every profile at once, because keys.json and credentials.json are shared by all of them:
//
//  1. Check: the agents' hook entries run the relay's own terma. With two installs it
//     does not migrate, and records why (ReadBlock), which doctor shows.
//  2. Prepare: each profile's routing state is written in the Preparing phase, with a
//     stable enrollment for its team, the bindings its validated entries need, and the
//     map from its legacy project to that enrollment (routing.Migration.Legacy). Whether
//     the team is a personal organization's is asked first, before any lock is taken,
//     with the profile's sign-in (Machine.OrgType): a personal team needs no bindings and
//     keeps a global policy, any other's global policy collects nothing.
//  3. Copy: the sign-ins to credentials.v2.json, which marks the machine migrated
//     (config.Migrated), then each profile's team key to keys.v2.json.
//  4. Retire: keys.json's keys, credentials.json's sign-ins and config.json's teams.
//  5. Commit: every routing state Committed.
//
// A failure before the sign-ins are copied leaves every legacy entry in place, so every
// reader goes on with today's rules, as after a failure before step 2; the next start
// prepares again, keeping the enrollments already chosen. A failure after it leaves a
// migrated machine whose routing states are Preparing, which collects nothing; the next
// start finishes from there, and never goes back to today's rules.
package migrate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// DefaultFlushWait bounds the wait for a spool flush already running: longer than a
// flush may take (spool.DefaultFlushTimeout), so one that is running ends first.
const DefaultFlushWait = spool.DefaultFlushTimeout + 5*time.Second

// Machine is what the relay migrates, and how it checks that it may.
type Machine struct {
	// ConfigDir and StateDir are terma's directories, as the command line resolved them.
	ConfigDir, StateDir string
	// Executable is the relay's own terma; "" is this process's (procinfo.AbsExecutable).
	Executable string
	// Hooked lists the termas the agents' machine-wide hook entries run, by path, as the
	// command line reads them from each agent's hooks file; nil lists none.
	Hooked func() ([]string, error)
	// OrgType is organizationID's type, OrgPersonal or OrgShared, as profile's own sign-in
	// to it lists it (GET /v1/organizations); the command line provides it. Nil asks
	// nothing, so a profile signed in to a team's organization is not migrated
	// (OrgTypeUnknown).
	OrgType func(ctx context.Context, profile, organizationID string) (string, error)
	// FlushWait bounds the wait for a spool flush already running; zero is
	// DefaultFlushWait.
	FlushWait time.Duration
	// Automatic migrates a machine whose developer has not opted in (OptIn) too.
	Automatic bool

	// fault, set by tests, fails the step it names as a crash just before it would.
	fault func(step string) error
}

// Outcome is what one Run did.
type Outcome string

// Outcomes.
const (
	// Done: the machine was migrated already, and Run changed nothing.
	Done Outcome = "done"
	// Committed: Run migrated the machine, or finished a migration an earlier start left.
	Committed Outcome = "committed"
	// NotMigrated: Run left the legacy entries as they were, for the reason ReadBlock
	// reports, and every reader goes on with today's rules. The next start tries again.
	NotMigrated Outcome = "not_migrated"
	// Unfinished: Run stopped after the machine was marked migrated and before every
	// routing state was Committed. It collects nothing until a start finishes it.
	Unfinished Outcome = "unfinished"
	// NotOptedIn: the machine is not migrated and its developer has not opted in (OptIn),
	// so Run asked, locked and recorded nothing, and every reader goes on with today's
	// rules.
	NotOptedIn Outcome = "not_opted_in"
)

// Run migrates the machine, or finishes a migration an earlier start left, while the
// caller holds the relay's lock; the relay runs it beside itself, once it listens. A
// machine not yet migrated moves only once its developer opted in (OptIn), or m.Automatic
// says to. It takes the spool flush's lock, waiting out a flush already running for at
// most m.FlushWait. While it runs it records that it does (Migrating). An error comes
// with NotMigrated or Unfinished, and is recorded for doctor too (ReadBlock), except one
// ctx ended, which changed nothing.
func Run(ctx context.Context, m Machine) (Outcome, error) {
	if !Needed(m.ConfigDir, m.StateDir) {
		return Done, nil
	}
	// A migration already marked on the machine is finished whatever was chosen: until
	// it is, the machine collects nothing.
	if !m.Automatic && !config.Migrated(m.ConfigDir) && !OptedIn(m.StateDir) {
		return NotOptedIn, nil
	}
	// Recorded first, and gone only once the outcome is: a block, or the commit.
	defer m.migrating()()
	// Asked before any lock is taken: network calls never hold one. A machine already
	// marked migrated only finishes what it prepared, which asks nothing.
	var personal map[string]bool
	if !config.Migrated(m.ConfigDir) {
		var err error
		if personal, err = m.orgTypes(ctx); err != nil {
			if ctx.Err() != nil {
				return NotMigrated, ctx.Err()
			}
			m.block(Block{Reason: OrgTypeUnknown, Detail: err.Error()})
			return NotMigrated, nil
		}
	}
	unlock, err := lockFlush(ctx, m.StateDir, cmp.Or(m.FlushWait, DefaultFlushWait))
	switch {
	case ctx.Err() != nil:
		return NotMigrated, ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		m.block(Block{Reason: FlushBusy, Detail: "a spool flush kept its lock past the wait"})
		return NotMigrated, nil
	case err != nil:
		m.block(Block{Reason: Failed, Detail: "take the spool flush's lock: " + err.Error()})
		return NotMigrated, err
	}
	defer unlock()
	if !Needed(m.ConfigDir, m.StateDir) {
		return Done, nil
	}
	if !config.Migrated(m.ConfigDir) {
		// Checked only while the legacy entries are all in place: once any has gone there
		// is no going back, whatever terma the hooks run.
		exe, others, err := m.otherInstalls()
		switch {
		case err != nil:
			m.block(Block{Reason: Unchecked, Detail: "could not tell which terma the agents' hooks run: " + err.Error(), Executable: exe})
			return NotMigrated, nil
		case len(others) > 0:
			m.block(Block{Reason: TwoInstalls, Detail: "the agents' hooks run another terma than the relay's", Executable: exe, Others: others})
			return NotMigrated, nil
		}
	}
	if err := m.run(personal); err != nil {
		if errors.Is(err, errOrgTypeUnknown) && !config.Migrated(m.ConfigDir) {
			m.block(Block{Reason: OrgTypeUnknown, Detail: err.Error()})
			return NotMigrated, nil
		}
		if config.Migrated(m.ConfigDir) {
			m.block(Block{Reason: Failed, Detail: err.Error(), Unfinished: true})
			return Unfinished, err
		}
		m.block(Block{Reason: Failed, Detail: err.Error()})
		return NotMigrated, err
	}
	clearBlock(m.StateDir)
	return Committed, nil
}

// Needed reports whether the machine has migrating left to do: it is not migrated, or a
// routing state is still Preparing. A relay start does the former only once the
// developer opted in (Run).
func Needed(configDir, stateDir string) bool {
	if !config.Migrated(configDir) {
		return true
	}
	states, _ := preparing(stateDir)
	return len(states) > 0
}

// lockFlush takes the spool flush's lock, waiting at most wait.
func lockFlush(ctx context.Context, stateDir string, wait time.Duration) (func(), error) {
	dir := filepath.Join(stateDir, spool.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return flock.Lock(ctx, filepath.Join(dir, spool.DeliveryLockFile))
}

// run is the migration's steps 2 to 5, from wherever an earlier start stopped. personal
// is whether each profile's team is a personal organization's (orgTypes).
func (m Machine) run(personal map[string]bool) error {
	if !config.Migrated(m.ConfigDir) {
		if err := m.prepare(personal); err != nil {
			return fmt.Errorf("prepare the routing states: %w", err)
		}
	}
	states, err := preparing(m.StateDir)
	if err != nil {
		return err
	}
	if err := m.step("copy-sign-ins", func() error { return auth.CopyToV2(m.ConfigDir) }); err != nil {
		return fmt.Errorf("copy the sign-ins: %w", err)
	}
	for _, s := range states {
		if len(s.Migration.Legacy) == 0 {
			continue
		}
		if err := m.step("copy-keys/"+s.Profile, func() error { return keystore.CopyLegacyToTeams(m.ConfigDir, s.Migration.Legacy) }); err != nil {
			return fmt.Errorf("copy profile %s's team key: %w", s.Profile, err)
		}
	}
	if err := m.step("retire-keys", func() error { return keystore.RetireV1(m.ConfigDir) }); err != nil {
		return fmt.Errorf("retire keys.json: %w", err)
	}
	if err := m.step("retire-sign-ins", func() error { return auth.RetireV1(m.ConfigDir) }); err != nil {
		return fmt.Errorf("retire credentials.json: %w", err)
	}
	if err := m.step("retire-teams", func() error { return retireTeams(m.ConfigDir) }); err != nil {
		return fmt.Errorf("retire config.json's teams: %w", err)
	}
	for _, s := range states {
		if err := m.step("commit/"+s.Profile, func() error { return commit(m.StateDir, s.Profile) }); err != nil {
			return fmt.Errorf("commit profile %s's routing state: %w", s.Profile, err)
		}
	}
	return nil
}

// step runs fn, unless a test's fault fails it first.
func (m Machine) step(name string, fn func() error) error {
	if m.fault != nil {
		if err := m.fault(name); err != nil {
			return err
		}
	}
	return fn()
}

// retireTeams clears the team from every profile in config.json, so an older terma finds
// none to collect for, and none to write back from a policy it could fetch.
func retireTeams(configDir string) error {
	if _, err := os.Stat(config.Path(configDir)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return config.UpdateFile(configDir, func(f *config.File) {
		for _, p := range f.Profiles {
			if p != nil {
				p.Team, p.TeamName = "", ""
			}
		}
	})
}

// commit marks profile's routing state Committed, if it is still Preparing.
func commit(stateDir, profile string) error {
	_, err := routing.Update(stateDir, profile, func(s *routing.State) error {
		if s.Migration != nil && s.Migration.Phase == routing.Preparing {
			s.Migration.Phase = routing.Committed
		}
		return nil
	})
	return err
}
