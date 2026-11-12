package routing

import (
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

// Update applies change to profile's routing state under its one lock: read, change,
// check, and write atomically with the next generation. A change that returns an error,
// or leaves the state broken, writes nothing.
func Update(stateDir, profile string, change func(*State) error) (State, error) {
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		s, _, err := load(path, profile)
		if err != nil {
			return err
		}
		s.Profile = profile
		if err := change(&s); err != nil {
			return err
		}
		if err := s.check(); err != nil {
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
