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
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

func aliasTarget(stateDir, id string) (string, error) {
	if !session.ValidID(id) {
		return "", fmt.Errorf("runs: invalid session id %q", id)
	}
	cur := id
	for range 8 {
		b, err := os.ReadFile(aliasPath(stateDir, cur))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return cur, nil
			}
			return "", err
		}
		target := strings.TrimSpace(string(b))
		if !session.ValidID(target) {
			return "", fmt.Errorf("runs: invalid alias %q", cur)
		}
		if target == cur {
			return cur, nil
		}
		cur = target
	}
	return "", fmt.Errorf("runs: alias cycle for %q", id)
}
func resolve(stateDir, idOrAlias string) (ledger, bool, bool, error) {
	target, err := aliasTarget(stateDir, idOrAlias)
	if err != nil || target == idOrAlias {
		if err != nil {
			return ledger{}, false, false, err
		}
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
	return withCanonical(stateDir, sessionID, "", try, func(_, path string, l *ledger) error { return edit(path, l) })
}
func linkAlias(stateDir, sessionID, alias string) error {
	if !session.ValidID(alias) {
		return fmt.Errorf("runs: invalid alias %q", alias)
	}
	return withCanonical(stateDir, sessionID, alias, false, func(canon, path string, l *ledger) error {
		dl, dok, _, err := readLedger(stateDir, alias)
		if err != nil {
			return err
		}
		changed := !slices.Contains(l.Aliases, alias)
		if changed {
			l.Aliases = append(l.Aliases, alias)
		}
		if dok && mergeMixed(l, dl) || changed {
			if err := save(path, l); err != nil {
				return err
			}
		}
		pointer := aliasPath(stateDir, alias)
		if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
			return err
		} else if err := config.WriteFileAtomic(pointer, []byte(canon), 0o600); err != nil {
			return err
		}
		if dl, dok, _, err := readLedger(stateDir, alias); err != nil {
			return err
		} else if dok && mergeMixed(l, dl) {
			return save(path, l)
		}
		return nil
	})
}
func withCanonical(stateDir, sessionID, extra string, try bool, fn func(canon, path string, l *ledger) error) error {
	for range 8 {
		canon, err := aliasTarget(stateDir, sessionID)
		if err != nil || extra == canon {
			return err
		}
		path, retry := filepath.Join(stateDir, Dir, canon+".json"), false
		run := func() error {
			next, err := aliasTarget(stateDir, canon)
			if err != nil || next != canon {
				sessionID, retry = next, err == nil
				return err
			}
			l, _, err := load(path)
			if err == nil {
				err = fold(path, &l)
			}
			if err == nil {
				err = fn(canon, path, &l)
			}
			return err
		}
		if extra != "" {
			p1, p2 := filepath.Join(stateDir, Dir, min(canon, extra)+".json"), filepath.Join(stateDir, Dir, max(canon, extra)+".json")
			err = flock.Locked(p1, lockWait, func() error { return flock.Locked(p2, lockWait, run) })
		} else if !try {
			err = flock.Locked(path, lockWait, run)
		} else if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			if unlock, uerr := flock.TryLock(path + ".lock"); uerr != nil {
				err = uerr
			} else {
				err = run()
				unlock()
			}
		}
		if err != nil || !retry {
			return err
		}
	}
	return fmt.Errorf("runs: alias loop for %q", sessionID)
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
	if err == nil {
		err = config.WriteFileAtomic(path, b, 0o600)
	}
	return err
}
func newRunID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
