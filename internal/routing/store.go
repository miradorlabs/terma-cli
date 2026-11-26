package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// ErrNoState is Update or Delete finding no routing state: only setup Creates or
// Replaces one, so a refresher never recreates a state that was lost.
var ErrNoState = errors.New("no routing state for this profile")

// ErrStateExists is Create finding an existing routing state for the profile.
var ErrStateExists = errors.New("profile already has a routing state")

// ErrAccountMismatch is a routing-state mutation finding that the state file on disk
// belongs to a different account than the caller expects (distinct from
// policy.ErrCredentialAccount, which reports a mismatched keychain credential during fetch).
var ErrAccountMismatch = errors.New("routing state belongs to another account")

// ErrStaleGeneration is a mutation finding that the routing state on disk changed since
// the caller read it.
var ErrStaleGeneration = errors.New("routing state changed since it was read")

// Expect names the account (required) and optional state instance ID and generation checks
// a routing state mutation requires on disk under the lock. Callers checking a generation
// should obtain Expect from State.Expect() or State.ExpectConsent() so StateID is populated.
type Expect struct {
	// Account is the required State.Account; empty or invalid values are refused.
	Account string
	// StateID, when non-empty, requires State.StateID == StateID (guards against ABA
	// matches after Delete+Create or Replace over a corrupt file). Required whenever
	// Generation or ConsentGeneration is non-nil.
	StateID string
	// Generation, when non-nil, requires State.Generation == *Generation.
	Generation *int64
	// ConsentGeneration, when non-nil, requires State.ConsentGeneration == *ConsentGeneration.
	ConsentGeneration *int64
}

func (e Expect) valid() error {
	if !ValidAccount(e.Account) {
		return fmt.Errorf("invalid expected account %q", e.Account)
	}
	if (e.Generation != nil || e.ConsentGeneration != nil) && e.StateID == "" {
		return errors.New("expected generation requires StateID (use State.Expect or State.ExpectConsent)")
	}
	return nil
}

func (e Expect) check(s State) error {
	if s.Account != e.Account {
		return fmt.Errorf("%w: on disk %s, expected %s", ErrAccountMismatch, s.Account, e.Account)
	}
	if e.StateID != "" && s.StateID != e.StateID {
		return fmt.Errorf("%w: state instance changed", ErrStaleGeneration)
	}
	if e.Generation != nil && s.Generation != *e.Generation {
		return fmt.Errorf("%w: on disk %d, expected %d", ErrStaleGeneration, s.Generation, *e.Generation)
	}
	if e.ConsentGeneration != nil && s.ConsentGeneration != *e.ConsentGeneration {
		return fmt.Errorf("%w: consent on disk %d, expected %d", ErrStaleGeneration, s.ConsentGeneration, *e.ConsentGeneration)
	}
	return nil
}

// StatePath is profile's routing state file under the state directory stateDir.
func StatePath(stateDir, profile string) (string, error) {
	if profile == "" || profile != filepath.Base(profile) || strings.HasPrefix(profile, ".") {
		return "", errors.New("invalid profile name for a routing state")
	}
	return filepath.Join(stateDir, StatesDir, profile+".json"), nil
}

// Load is profile's routing state under stateDir; false when there is none. A state that
// cannot be read, belongs to another profile, or carries an invalid account is an error:
// it grants nothing, and it is never rebuilt from policy files.
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

// Update applies change to profile's existing routing state under its one lock if on-disk
// state satisfies expect: read, verify expect, change, check, and write atomically with
// the next generation (and next ConsentGeneration when Teams membership/order/identity
// changed). ErrNoState when there is none; ErrAccountMismatch or
// ErrStaleGeneration when expect is not met. A change that returns an error, alters
// Profile/Account/StateID/Generation/ConsentGeneration, leaves the state broken, or
// changes nothing writes nothing.
func Update(stateDir, profile string, expect Expect, change func(*State) error) (State, error) {
	if err := expect.valid(); err != nil {
		return State{}, err
	}
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		s, ok, err := load(path, profile)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoState
		}
		if err := expect.check(s); err != nil {
			return err
		}
		origGen, origConsent, origID := s.Generation, s.ConsentGeneration, s.StateID
		before, err := json.Marshal(s)
		if err != nil {
			return err
		}
		beforeConsent := consentFingerprint(s)
		if err := change(&s); err != nil {
			return err
		}
		if s.Profile != profile || s.Account != expect.Account || s.StateID != origID || s.Generation != origGen || s.ConsentGeneration != origConsent {
			return errors.New("routing update may not change profile, account, state_id, generation, or consent_generation")
		}
		if err := s.check(); err != nil {
			return err
		}
		after, err := json.Marshal(s)
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			out = s
			return nil
		}
		s.Generation = origGen + 1
		if !beforeConsent.equal(consentFingerprint(s)) {
			s.ConsentGeneration = origConsent + 1
		}
		return writeAtomic(path, s, &out)
	})
	return out, err
}

// Create writes profile's first routing state, stamped with account and a fresh StateID
// and built by change from an empty State; ErrStateExists if one already exists.
func Create(stateDir, profile, account string, change func(*State) error) (State, error) {
	if !ValidAccount(account) {
		return State{}, fmt.Errorf("invalid account %q", account)
	}
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		_, ok, err := load(path, profile)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: %s", ErrStateExists, profile)
		}
		return writeFresh(path, profile, account, 1, 1, change, &out)
	})
	return out, err
}

// Replace atomically writes a fresh routing state stamped with newAccount and a fresh
// StateID under the lock. When expectPrev is non-nil, an existing readable state matching
// expectPrev is required (failing closed on missing, corrupt, wrong account, or stale
// generation). When expectPrev is nil, any missing, existing, or unreadable prior state
// is replaced, advancing Generation and ConsentGeneration past any readable prior values.
func Replace(stateDir, profile string, expectPrev *Expect, newAccount string, change func(*State) error) (State, error) {
	if !ValidAccount(newAccount) {
		return State{}, fmt.Errorf("invalid new account %q", newAccount)
	}
	if expectPrev != nil {
		if err := expectPrev.valid(); err != nil {
			return State{}, fmt.Errorf("invalid expected previous state: %w", err)
		}
	}
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		prev, ok, err := load(path, profile)
		var nextGen, nextConsent int64 = 1, 1
		if expectPrev != nil {
			if err != nil {
				return err
			}
			if !ok {
				return ErrNoState
			}
			if err := expectPrev.check(prev); err != nil {
				return err
			}
			nextGen, nextConsent = prev.Generation+1, prev.ConsentGeneration+1
		} else if err == nil && ok {
			nextGen, nextConsent = prev.Generation+1, prev.ConsentGeneration+1
		}
		return writeFresh(path, profile, newAccount, nextGen, nextConsent, change, &out)
	})
	return out, err
}

// ReplaceCorrupt atomically replaces a corrupt or unreadable routing state file with a
// fresh state stamped with newAccount under the lock. Returns ErrStateExists if a valid
// state now exists on disk, or ErrNoState if the file was removed concurrently.
func ReplaceCorrupt(stateDir, profile, newAccount string, change func(*State) error) (State, error) {
	if !ValidAccount(newAccount) {
		return State{}, fmt.Errorf("invalid new account %q", newAccount)
	}
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return State{}, err
	}
	var out State
	err = flock.Locked(path, lockWait, func() error {
		_, ok, err := load(path, profile)
		if err == nil {
			if ok {
				return fmt.Errorf("%w: %s", ErrStateExists, profile)
			}
			return ErrNoState
		}
		return writeFresh(path, profile, newAccount, 1, 1, change, &out)
	})
	return out, err
}

func writeFresh(path, profile, account string, nextGen, nextConsent int64, change func(*State) error, out *State) error {
	stateID, err := NewEnrollmentID()
	if err != nil {
		return err
	}
	s := State{StateID: stateID, Profile: profile, Account: account}
	if err := change(&s); err != nil {
		return err
	}
	if s.Profile != profile || s.Account != account || s.StateID != stateID || s.Generation != 0 || s.ConsentGeneration != 0 {
		return errors.New("routing write may not change profile, account, state_id, generation, or consent_generation")
	}
	if err := s.check(); err != nil {
		return err
	}
	s.Generation = nextGen
	s.ConsentGeneration = nextConsent
	return writeAtomic(path, s, out)
}

type teamConsent struct {
	EnrollmentID   string
	OrganizationID string
	AuthURL        string
	TeamID         string
	Personal       bool
}

type consentState []teamConsent

func consentFingerprint(s State) consentState {
	cs := make(consentState, len(s.Teams))
	for i, t := range s.Teams {
		cs[i] = teamConsent{
			EnrollmentID:   t.EnrollmentID,
			OrganizationID: t.OrganizationID,
			AuthURL:        t.AuthURL,
			TeamID:         t.TeamID,
			Personal:       t.Personal,
		}
	}
	return cs
}

func (a consentState) equal(b consentState) bool { return slices.Equal(a, b) }

// Delete removes profile's routing state under the lock if the on-disk state matches
// expect. Returns ErrNoState if absent, or ErrAccountMismatch/ErrStaleGeneration if
// expect is not satisfied. A corrupt state file fails to load and cannot be deleted
// through Delete; recover or reset it via Replace(stateDir, profile, nil, ...).
func Delete(stateDir, profile string, expect Expect) error {
	if err := expect.valid(); err != nil {
		return err
	}
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return err
	}
	return flock.Locked(path, lockWait, func() error {
		s, ok, err := load(path, profile)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoState
		}
		if err := expect.check(s); err != nil {
			return err
		}
		return os.Remove(path)
	})
}

// DeleteCorrupt removes a corrupt or unreadable routing state file under the lock.
// Returns ErrNoState if the file is already absent, or ErrStaleGeneration if a valid
// routing state now exists on disk (requiring a CAS Delete with its Expect).
func DeleteCorrupt(stateDir, profile string) error {
	path, err := StatePath(stateDir, profile)
	if err != nil {
		return err
	}
	return flock.Locked(path, lockWait, func() error {
		_, ok, err := load(path, profile)
		if err == nil {
			if !ok {
				return ErrNoState
			}
			return ErrStaleGeneration
		}
		return os.Remove(path)
	})
}

func writeAtomic(path string, s State, out *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, b, 0o600); err != nil {
		return err
	}
	*out = s
	return nil
}
