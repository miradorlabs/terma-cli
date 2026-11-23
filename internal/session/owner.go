package session

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
)

// A session's owner is the account its first write claimed, in a file of its own beside its
// manifest, <id>.owner, made whole in one step that fails once the file exists (claim). Every
// write claims before it records anything, locked or not, so the first claim decides whose the
// session is, and a delta can't name anyone else. Its account is the session's for good: ""
// names no one, and is never filled in.
const ownerExt = ".owner"

// owner is what k's owner file says: claimed is false when it has none.
type owner struct {
	account string
	claimed bool
}

func (s *Store) ownerPath(k Key) string {
	return filepath.Join(s.dir, manifestsDir, k.Tool, k.ID+ownerExt)
}

// errEmptyOwner marks a zero-length owner file, which can occur briefly during the
// O_CREATE|O_EXCL fallback or after a crash; readers fail closed, while claim retries briefly.
var errEmptyOwner = errors.New("empty owner file")

// owner reads k's owner file. One that can't be read names an owner no one can check, an
// error: whoever reads it fails closed.
func (s *Store) owner(k Key) (owner, error) {
	if !k.valid() {
		return owner{}, fmt.Errorf("invalid session %q of tool %q", k.ID, k.Tool)
	}
	data, err := os.ReadFile(s.ownerPath(k))
	if errors.Is(err, fs.ErrNotExist) {
		return owner{}, nil
	}
	if err != nil {
		return owner{}, err
	}
	if len(data) == 0 {
		return owner{}, fmt.Errorf("owner of %s: %w", k.ID, errEmptyOwner)
	}
	if data[0] != '"' {
		return owner{}, fmt.Errorf("owner of %s: not a JSON string", k.ID)
	}
	var account string
	if err := json.Unmarshal(data, &account); err != nil {
		return owner{}, fmt.Errorf("owner of %s: %w", k.ID, err)
	}
	return owner{account: account, claimed: true}, nil
}

// claim is the account k's session is recorded under: the one its owner file names, or, with
// none yet, the Store's, claimed for it. Two writers that both find none both try, and the
// file names the one that made it first; the other reads it. A session recorded before it had
// an owner file is claimed for the account its manifest names, never the writer's.
// Errors reading an existing owner or manifest, or exhausting retries, wrap
// ErrAccountConflict (fail closed); an I/O error creating a new owner file is returned
// unwrapped so a read-only store is not miscounted as an account conflict.
func (s *Store) claim(k Key) (string, error) {
	for range 8 {
		o, err := s.owner(k)
		if errors.Is(err, errEmptyOwner) {
			if s.emptyOwnerWait != nil {
				s.emptyOwnerWait()
			} else {
				time.Sleep(time.Millisecond)
			}
			continue
		}
		if err != nil {
			return "", unreadableOwner(err)
		}
		if o.claimed {
			return o.account, nil
		}
		m, _, err := s.load(k)
		if err != nil {
			return "", unreadableOwner(err)
		}
		account := s.account
		if m != nil {
			account = m.Account
		}
		// ErrExist: another writer claimed it first. ErrNotExist: a Retire took the folder.
		switch err := s.link(k, account); {
		case err == nil:
			return account, nil
		case !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist):
			if m != nil && conflicts(m.Account, s.account) {
				return "", ErrAccountConflict
			}
			return "", err
		}
	}
	return "", unreadableOwner(fmt.Errorf("the owner of %s could not be claimed", k.ID))
}

// link makes k's owner file name account, whole, or fails with fs.ErrExist if it exists: the
// file is written aside, synced, then linked to its name, which a link never replaces. On a
// filesystem or sandbox that refuses hard links, it falls back to O_CREATE|O_EXCL.
func (s *Store) link(k Key, account string) error {
	path := s.ownerPath(k)
	data, err := json.Marshal(account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), config.TempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	err = os.Link(f.Name(), path)
	if err == nil || errors.Is(err, fs.ErrExist) || errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeExclusive(path, data)
}

// writeExclusive creates path with O_CREATE|O_EXCL and writes data, removing path if the
// write fails. Used only when os.Link is refused by the filesystem or sandbox.
func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (s *Store) hasDeltas(k Key) bool {
	deltas, _ := filepath.Glob(filepath.Join(s.dir, manifestsDir, k.Tool, k.ID+deltaSep+"*"+deltaExt))
	return len(deltas) > 0
}

// pruneOwners removes the owner files of sessions with nothing else left, no manifest and no
// delta, claimed before before: what a Merge, or a writer killed after its claim, left. A
// claim younger than that may be a writer's about to record.
func (s *Store) pruneOwners(before time.Time) {
	paths, _ := filepath.Glob(filepath.Join(s.dir, manifestsDir, "*", "*"+ownerExt))
	for _, p := range paths {
		k := Key{Tool: filepath.Base(filepath.Dir(p)), ID: strings.TrimSuffix(filepath.Base(p), ownerExt)}
		if !k.valid() || fileExists(s.manifestPath(k)) || s.hasDeltas(k) {
			continue
		}
		if info, err := os.Stat(p); err == nil && info.ModTime().Before(before) {
			_ = os.Remove(p)
		}
	}
}
