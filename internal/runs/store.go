package runs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// How the ledger is stored: a file per session, read whole and replaced atomically under
// its lock, with its sidecar (aside.go) applied on every read.

// resolve reads the ledger idOrAlias names, its own, else its session's through its alias,
// with that session's sidecar applied (MarkAside); aside reports a sidecar. The sidecar is
// read first: a fold saves the ledger before it removes what it folded.
func resolve(stateDir, idOrAlias string) (l ledger, ok, aside bool, err error) {
	read := func(sessionID string) error {
		a, err := readAside(stateDir, sessionID)
		if err != nil {
			return err
		}
		path, err := ledgerPath(stateDir, sessionID)
		if err != nil {
			return err
		}
		if l, ok, err = load(path); err != nil {
			return err
		}
		if len(a.marks) > 0 {
			a.apply(&l)
			ok, aside = true, true
		}
		return nil
	}
	if err := read(idOrAlias); err != nil || ok {
		return l, ok, aside, err
	}
	b, err := os.ReadFile(aliasPath(stateDir, idOrAlias))
	if errors.Is(err, fs.ErrNotExist) {
		return ledger{}, false, false, nil
	}
	if err != nil {
		return ledger{}, false, false, err
	}
	err = read(strings.TrimSpace(string(b)))
	return l, ok, aside, err
}

// update applies change to sessionID's ledger, made if there is none, under its lock.
func update(stateDir, sessionID string, change func(*ledger)) error {
	return lockedEdit(stateDir, sessionID, func(path string, l *ledger) error {
		change(l)
		return save(path, l)
	})
}

// lockedEdit runs edit on sessionID's ledger, empty if there is none, holding its lock, its
// sidecar folded in first (fold).
func lockedEdit(stateDir, sessionID string, edit func(path string, l *ledger) error) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	return flock.Locked(path, lockWait, func() error { return editLoaded(stateDir, sessionID, path, edit) })
}

// tryEdit is lockedEdit only if the lock is free now: it never waits.
func tryEdit(stateDir, sessionID string, edit func(path string, l *ledger) error) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := flock.TryLock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return editLoaded(stateDir, sessionID, path, edit)
}

// editLoaded loads the ledger at path, folds its sidecar in and runs edit on it; the caller
// holds its lock.
func editLoaded(stateDir, sessionID, path string, edit func(path string, l *ledger) error) error {
	l, _, err := load(path)
	if err != nil {
		return err
	}
	if err := fold(stateDir, sessionID, path, &l); err != nil {
		return err
	}
	return edit(path, &l)
}

func ledgerPath(stateDir, sessionID string) (string, error) {
	if !session.ValidID(sessionID) {
		return "", fmt.Errorf("runs: invalid session id %q", sessionID)
	}
	return filepath.Join(stateDir, Dir, sessionID+".json"), nil
}

func aliasPath(stateDir, alias string) string {
	return filepath.Join(stateDir, Dir, aliasesDir, alias)
}

func load(path string) (ledger, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ledger{}, false, nil
	}
	if err != nil {
		return ledger{}, false, err
	}
	var l ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return ledger{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return l, true, nil
}

// save writes l durably: a crossing recorded here must survive a crash.
func save(path string, l *ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
