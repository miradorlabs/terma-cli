// Package keystore holds the project server keys the spool flushes with.
//
// A harness's key lives inside that harness's own config; the spool is terma's
// and needs its own copy, keyed by project, so a flush can deliver events for any
// repository this machine has set up. One 0600 file under the config dir, never
// printed: `terma status` shows the masked prefix only.
package keystore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/serverkey"
)

const fileName = "keys.json"

type file struct {
	// Keys maps project id → Terma server key.
	Keys map[string]string `json:"keys"`
	// HarnessKeys maps harness name → project id → the key that harness exported with
	// when it was last pointed at that project. Each harness holds a key of its own so
	// one can be revoked without the other; remembering them here means re-pointing a
	// harness at a project it reported to before reuses its own key instead of minting
	// another every time the developer moves between repositories.
	HarnessKeys map[string]map[string]string `json:"harness_keys,omitempty"`
}

func path() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fileName), nil
}

func load() (*file, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &file{Keys: map[string]string{}, HarnessKeys: map[string]map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	if f.Keys == nil {
		f.Keys = map[string]string{}
	}
	if f.HarnessKeys == nil {
		f.HarnessKeys = map[string]map[string]string{}
	}
	return &f, nil
}

// lockWait bounds the wait for another terma's update. Holders rewrite one small file.
const lockWait = 5 * time.Second

// update is the one way the file changes: load, edit, save, under a lock. Two installs
// in two repositories each used to read the file, add their project's key and rename
// their copy back, and the second rename forgot the first key — after which that
// project's hook events were held for want of one.
func update(edit func(*file)) error {
	p, err := path()
	if err != nil {
		return err
	}
	// The first key can be the first thing terma ever writes: `terma install` mints one
	// before any credential or profile has created the config directory.
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	unlock, err := flock.Lock(ctx, p+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := load()
	if err != nil {
		return err
	}
	edit(f)
	return save(f)
}

func save(f *file) error {
	p, err := path()
	if err != nil {
		return err
	}
	return config.WriteJSON(p, f, 0o600)
}

// Get returns the key for a project, or "".
func Get(projectID string) string {
	f, err := load()
	if err != nil {
		return ""
	}
	key := f.Keys[projectID]
	if !serverkey.Is(key) {
		return ""
	}
	return key
}

// Set records a project's key.
func Set(projectID, key string) error {
	if projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a project id and a server key are required")
	}
	return update(func(f *file) { f.Keys[projectID] = key })
}

// Projects lists the project ids with a stored key.
func Projects() []string {
	f, err := load()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(f.Keys))
	for id := range f.Keys {
		out = append(out, id)
	}
	return out
}

// Mask shows the key's prefix only, for status output.
func Mask(key string) string { return serverkey.Mask(key) }

// GetFor returns the key a harness last exported with for a project, or "".
func GetFor(harness, projectID string) string {
	f, err := load()
	if err != nil {
		return ""
	}
	key := f.HarnessKeys[harness][projectID]
	if !serverkey.Is(key) {
		return ""
	}
	return key
}

// SetFor records the key a harness exports with for a project, alongside the
// project's spool key.
func SetFor(harness, projectID, key string) error {
	if harness == "" || projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a harness, a project id and a server key are required")
	}
	return update(func(f *file) {
		if f.HarnessKeys[harness] == nil {
			f.HarnessKeys[harness] = map[string]string{}
		}
		f.HarnessKeys[harness][projectID] = key
		f.Keys[projectID] = key
	})
}
