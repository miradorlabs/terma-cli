package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// StatesDir names the state directory's folder of routing states, one file per profile.
const StatesDir = "routing"

// lockWait bounds how long a writer waits for another: every write is a read, a small
// merge and a rename, and network calls happen before the lock is taken.
const lockWait = 5 * time.Second

// ErrNoState is Update finding no routing state: only setup and the migration Create one,
// so a refresher never recreates a state that was lost.
var ErrNoState = errors.New("no routing state for this profile")

// StatePath is profile's routing state file under the state directory stateDir.
func StatePath(stateDir, profile string) (string, error) {
	if profile == "" || profile != filepath.Base(profile) || strings.HasPrefix(profile, ".") {
		return "", errors.New("invalid profile name for a routing state")
	}
	return filepath.Join(stateDir, StatesDir, profile+".json"), nil
}

// Load is profile's routing state under stateDir; false when there is none. A state that
// cannot be read, or that belongs to another profile, is an error: it grants nothing,
// and it is never rebuilt from policy files.
func Load(stateDir, profile string) (State, bool, error) {
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, false, err
	}
	return load(path, profile)
}

// LoadReady is profile's routing state when it may route (State.Ready): false when there
// is none, and when the migration has not committed it yet. Hooks, the relay and delivery
// read their state through it, so a Preparing state sends nothing whichever reads it;
// only the migration, doctor and `terma teams` read one that is not ready, with Load.
func LoadReady(stateDir, profile string) (State, bool, error) {
	s, ok, err := Load(stateDir, profile)
	if err != nil || !ok || !s.Ready() {
		return State{}, false, err
	}
	return s, true, nil
}

func load(path, profile string) (State, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Profile != profile {
		return State{}, false, fmt.Errorf("%s belongs to profile %q", path, s.Profile)
	}
	if err := s.check(); err != nil {
		return State{}, false, fmt.Errorf("invalid routing state in %s: %w", path, err)
	}
	return s, true, nil
}

// Update applies change to profile's existing routing state under its one lock: read,
// change, check, and write atomically with the next generation. ErrNoState when there is
// none. A change that returns an error, leaves the state broken, or changes nothing
// writes nothing.
func Update(stateDir, profile string, change func(*State) error) (State, error) {
	return write(stateDir, profile, false, change)
}

// Create writes profile's first routing state, made by change from an empty one; an
// error if one exists. Setup and the migration are its only callers.
func Create(stateDir, profile string, change func(*State) error) (State, error) {
	return write(stateDir, profile, true, change)
}

func write(stateDir, profile string, create bool, change func(*State) error) (State, error) {
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		s, ok, err := load(path, profile)
		switch {
		case err != nil:
			return err
		case !ok && !create:
			return ErrNoState
		case ok && create:
			return fmt.Errorf("profile %s already has a routing state", profile)
		}
		s.Profile = profile
		before, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if err := change(&s); err != nil {
			return err
		}
		if err := s.check(); err != nil {
			return err
		}
		if after, err := json.Marshal(s); err != nil || ok && bytes.Equal(before, after) {
			out = s
			return err
		}
		s.Generation++
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		out = s
		return config.WriteFileAtomic(path, b, 0o600)
	})
	return out, err
}
