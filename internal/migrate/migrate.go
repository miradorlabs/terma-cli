// Package migrate brings state an earlier terma left in the config directory up to what
// this build reads, once per machine, before any command (hooks included) reads it.
// Migrations have append-only IDs, are idempotent, recognize the old shape exactly,
// leave state the previous build can read, and touch the home directory only.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Migration is one change to the state under the config directory.
type Migration struct {
	ID   int
	Name string
	// Run stops early when ctx is done, leaving state the next start can finish from.
	Run func(ctx context.Context) error
}

// State is what migrations.json records.
type State struct {
	// Applied is the ID of the last migration that completed.
	Applied int      `json:"applied"`
	Failed  *Failure `json:"failed,omitempty"`
}

// Failure is a migration that returned an error.
type Failure struct {
	ID    int       `json:"id"`
	Name  string    `json:"name"`
	At    time.Time `json:"at"`
	Error string    `json:"error"`
}

// RetryAfter is how long an ordinary start leaves a failed migration alone, so hooks do
// not repeat it on every tool call.
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

// Pending reports whether this build has migrations the existing state under dir has not had.
func Pending(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	s, err := Load(dir)
	return err != nil || s.Applied < Latest()
}

// Run applies pending migrations in order under one lock and returns their names; it
// records the first failure and stops (retry ignores RetryAfter). A run ctx cuts short
// records no failure.
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
		// Unreadable: running everything again is safe, and the rewrite repairs it.
		s = State{}
	}
	if f := s.Failed; f != nil && !slices.ContainsFunc(migrations, func(m Migration) bool { return m.ID == f.ID }) {
		s.Failed = nil
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
