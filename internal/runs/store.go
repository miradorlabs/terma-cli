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

func aliasTarget(stateDir, id string) (string, error) {
	if !session.ValidID(id) {
		return "", fmt.Errorf("runs: invalid session id %q", id)
	}
	b, err := os.ReadFile(aliasPath(stateDir, id))
	if errors.Is(err, fs.ErrNotExist) {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	if target := strings.TrimSpace(string(b)); session.ValidID(target) {
		return target, nil
	}
	return "", fmt.Errorf("runs: invalid alias %q", id)
}

// resolve reads idOrAlias (preferring aliases/<idOrAlias> when present) with sidecar read first.
func resolve(stateDir, idOrAlias string) (ledger, bool, bool, error) {
	target, err := aliasTarget(stateDir, idOrAlias)
	if err != nil {
		return ledger{}, false, false, err
	}
	if target == idOrAlias {
		return readLedger(stateDir, idOrAlias)
	}
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

func readLedger(stateDir, id string) (ledger, bool, bool, error) {
	path := filepath.Join(stateDir, Dir, id+".json")
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

func mergeMixed(dst *ledger, src ledger) (merged bool) {
	for _, r := range src.Runs {
		if !r.MixedAt.IsZero() {
			if len(dst.Runs) == 0 {
				dst.Runs = append(dst.Runs, r)
			} else {
				dst.Runs[inForceIdx(dst.Runs, r.MixedAt)].mark(r.MixedAt, r.why())
			}
			dst.active(r.MixedAt)
			merged = true
		}
	}
	return merged
}

func update(stateDir, sessionID string, change func(*ledger)) error {
	return editLedger(stateDir, sessionID, false, func(path string, l *ledger) error { change(l); return save(path, l) })
}

func editLedger(stateDir, sessionID string, try bool, edit func(path string, l *ledger) error) error {
	target, err := aliasTarget(stateDir, sessionID)
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, Dir, target+".json")
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
func save(path string, l *ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}
func newRunID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
