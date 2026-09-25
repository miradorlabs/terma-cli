// Package migrate brings the state an earlier terma left in the config directory up to
// what this build reads, so an update needs nothing more from the developer.
//
// A migration runs once per machine, in order, the first time a newer build starts —
// whatever command that is, hooks included — before the command reads any state. The
// last one applied is recorded in migrations.json, one small read on every start.
//
// The rules a migration keeps, because of where it runs:
//
//   - Append-only IDs. Never renumber, reuse or delete one: a machine may have run it.
//   - Idempotent. A crash between the change and the record runs it again.
//   - Readable by the previous build. Another terma may share the machine (doctor warns
//     when one does) and read the same files next, so a migration adds and fills in; it
//     never removes or repurposes. Dropping what nothing reads any more is a later
//     migration's job, once no supported build reads it.
//   - Precise. Recognize the old shape exactly (a missing key, not a false one) and leave
//     anything else alone: a fresh machine runs every migration against state this
//     build wrote.
//   - Home directory only. A repository's committed files are shared with colleagues on
//     other versions; `terma update --refresh` rewrites those, on request.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Migration is one change to the state under the config directory.
type Migration struct {
	// ID orders migrations and records them as done.
	ID int
	// Name says what it changes, for messages.
	Name string
	// Run makes the change. It stops early when ctx is done, leaving state the next
	// start can finish from; that is how a hook keeps to its bound.
	Run func(ctx context.Context) error
}

// State is what migrations.json records.
type State struct {
	// Applied is the ID of the last migration that completed.
	Applied int `json:"applied"`
	// Failed is the migration that stopped the last run, until one gets past it.
	Failed *Failure `json:"failed,omitempty"`
}

// Failure is a migration that returned an error.
type Failure struct {
	ID    int       `json:"id"`
	Name  string    `json:"name"`
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

// RetryAfter is how long a start that is not asked to retry leaves a failed migration
// alone, so a hook firing on every tool call does not repeat a failure each time.
const RetryAfter = 15 * time.Minute

const (
	stateFile = "migrations.json"
	lockFile  = "migrations.lock"
)

// Latest is the ID of the newest migration this build has.
func Latest() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].ID
}

// Load reads the recorded state. A missing file is a machine that has run none.
func Load(dir string) (State, error) {
	var s State
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// Remaining counts the migrations this build has that s has not had.
func Remaining(s State) int {
	n := 0
	for _, m := range migrations {
		if m.ID > s.Applied {
			n++
		}
	}
	return n
}

// Pending reports whether this build has migrations the state under dir has not had.
// A config directory that does not exist has nothing to migrate. A newer build's record
// (an ID beyond this build's) is not pending: there is nothing an older build can add.
func Pending(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	s, err := Load(dir)
	return err != nil || s.Applied < Latest()
}

// Run applies the pending migrations in order, recording each as it completes, and
// returns the names of those it applied. It stops at the first failure, which it
// records; a later run starts again from there. retry says whether to attempt a
// migration that failed less than RetryAfter ago. Concurrent starts wait on one lock,
// and the ones that get it second find nothing left to do.
//
// ctx bounds the whole run, not only the wait for the lock: it is checked before each
// migration and passed into it. A run that ctx cuts short is not a failure — nothing is
// recorded against the migration, and the next start carries on where it stopped.
func Run(ctx context.Context, dir string, retry bool) ([]string, error) {
	if !Pending(dir) {
		return nil, nil
	}
	unlock, err := flock.Lock(ctx, filepath.Join(dir, lockFile))
	if err != nil {
		return nil, fmt.Errorf("wait for another terma to finish migrating: %w", err)
	}
	defer unlock()
	s, err := Load(dir)
	if err != nil {
		// Unreadable is not "none applied": running everything again is safe (each
		// migration is idempotent), and rewriting the record repairs it.
		s = State{}
	}
	if f := s.Failed; f != nil && !retry && time.Since(f.At) < RetryAfter {
		return nil, fmt.Errorf("%s: %s", f.Name, f.Error)
	}
	var applied []string
	for _, m := range migrations {
		if m.ID <= s.Applied {
			continue
		}
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		if err := m.Run(ctx); err != nil {
			if ctx.Err() != nil {
				return applied, fmt.Errorf("%s: %w", m.Name, ctx.Err())
			}
			s.Failed = &Failure{ID: m.ID, Name: m.Name, At: time.Now(), Error: err.Error()}
			_ = save(dir, s)
			return applied, fmt.Errorf("%s: %w", m.Name, err)
		}
		s.Applied, s.Failed = m.ID, nil
		if err := save(dir, s); err != nil {
			return applied, err
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

func save(dir string, s State) error {
	return config.WriteJSON(filepath.Join(dir, stateFile), s, 0o600)
}
