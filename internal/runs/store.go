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

// resolve reads idOrAlias (or its session via alias) with sidecar read first so a
// concurrent fold (save ledger before removing sidecar) never hides a Mixed mark.
func resolve(stateDir, idOrAlias string) (l ledger, ok, aside bool, err error) {
	read := func(sessionID string) error {
		path, err := ledgerPath(stateDir, sessionID)
		if err != nil {
			return err
		}
		a, err := loadAside(asidePath(path))
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
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ledger{}, false, false, nil
		}
		return ledger{}, false, false, err
	}
	err = read(strings.TrimSpace(string(b)))
	return l, ok, aside, err
}

func update(stateDir, sessionID string, change func(*ledger)) error {
	return editLedger(stateDir, sessionID, false, func(path string, l *ledger) error {
		change(l)
		return save(path, l)
	})
}

func editLedger(stateDir, sessionID string, try bool, edit func(path string, l *ledger) error) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	run := func() error {
		l, _, err := load(path)
		if err != nil {
			return err
		}
		if err := fold(path, &l); err != nil {
			return err
		}
		return edit(path, &l)
	}
	if !try {
		return flock.Locked(path, lockWait, run)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := flock.TryLock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return run()
}

func ledgerPath(stateDir, sessionID string) (string, error) {
	if !session.ValidID(sessionID) {
		return "", fmt.Errorf("runs: invalid session id %q", sessionID)
	}
	return filepath.Join(stateDir, Dir, sessionID+".json"), nil
}

func aliasPath(stateDir, alias string) string { return filepath.Join(stateDir, Dir, aliasesDir, alias) }

func load(path string) (ledger, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		return ledger{}, false, err
	}
	var l ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return ledger{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return l, true, nil
}

func save(path string, l *ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

func newRunID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
