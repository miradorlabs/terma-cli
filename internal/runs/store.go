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

// resolve reads idOrAlias (preferring aliases/<idOrAlias> when present) with sidecar read first.
// If both an alias target and a direct ledger exist, any Mixed mark from the direct ledger is merged.
func resolve(stateDir, idOrAlias string) (ledger, bool, bool, error) {
	if b, err := os.ReadFile(aliasPath(stateDir, idOrAlias)); err == nil {
		target := strings.TrimSpace(string(b))
		if target == "" {
			return ledger{}, false, false, fmt.Errorf("runs: empty alias %q", idOrAlias)
		}
		if target != idOrAlias {
			tl, tok, ta, err := readLedger(stateDir, target)
			if err != nil {
				return ledger{}, false, false, err
			}
			dl, dok, da, err := readLedger(stateDir, idOrAlias)
			if err != nil || !tok {
				return dl, dok, da, err
			}
			if dok {
				mergeMixed(&tl, dl)
			}
			return tl, true, ta || da, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ledger{}, false, false, err
	}
	return readLedger(stateDir, idOrAlias)
}

func readLedger(stateDir, id string) (ledger, bool, bool, error) {
	path, err := ledgerPath(stateDir, id)
	if err != nil {
		return ledger{}, false, false, err
	}
	a, err := loadAside(asidePath(path))
	if err != nil {
		return ledger{}, false, false, err
	}
	l, ok, err := load(path)
	if err != nil || len(a.marks) == 0 {
		return l, ok, false, err
	}
	a.apply(&l)
	return l, true, true, nil
}

func mergeMixed(dst *ledger, src ledger) {
	for _, r := range src.Runs {
		if !r.MixedAt.IsZero() {
			if len(dst.Runs) == 0 {
				dst.Runs = append(dst.Runs, r)
			} else {
				dst.Runs[inForceIdx(dst.Runs, r.MixedAt)].mark(r.MixedAt, r.why())
			}
			dst.active(r.MixedAt)
		}
	}
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
		if err == nil {
			err = fold(path, &l)
		}
		if err != nil {
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
			return ledger{}, false, nil
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
