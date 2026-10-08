// Package keystore holds the project server keys the spool flushes with, keyed by project
// so a flush can deliver for any repository on this machine. keys.json (0600) indexes them
// with their hosts; each key lives in the system keychain (internal/account/secret), or in
// the file itself where there is none or config.json asks for files.
package keystore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

const fileName = "keys.json"

// file is keys.json. A key the keychain holds is an empty value: the entry alone says
// the project has one.
type file struct {
	Keys map[string]string `json:"keys"`
	// HarnessKeys maps harness → project → its last key, so re-pointing a harness reuses
	// its own key instead of minting another.
	HarnessKeys map[string]map[string]string `json:"harness_keys,omitempty"`
	// Hosts maps project → its key's environment, which alone accepts the key; a missing
	// entry means "not recorded".
	Hosts map[string]Hosts `json:"hosts,omitempty"`
	// Stale names keychain items that may still hold a key this file now holds itself;
	// sweepStale removes them once the file is on disk.
	Stale []string `json:"stale_keychain_items,omitempty"`
	// added are the items this write gave a key the file on disk does not index there: if
	// the file is not saved, nothing else could find them.
	added []string
}

func (f *file) mark(item string) {
	if !slices.Contains(f.Stale, item) {
		f.Stale = append(f.Stale, item)
	}
}

func (f *file) unmark(item string) {
	f.Stale = slices.DeleteFunc(f.Stale, func(s string) bool { return s == item })
}

// live reports whether item holds a key the file indexes there now.
func (f *file) live(item string) bool {
	for id, v := range f.Keys {
		if v == "" && projectItem(id) == item {
			return true
		}
	}
	for harness, keys := range f.HarnessKeys {
		for id, v := range keys {
			if v == "" && harnessItem(harness, id) == item {
				return true
			}
		}
	}
	return false
}

// Hosts are the ingest and data API hosts of the environment a project's key belongs to.
type Hosts struct {
	// Env names a built-in environment, resolved through the current table on read so a
	// renamed host does not strand its keys.
	Env  string `json:"env,omitempty"`
	OTLP string `json:"otlp,omitempty"`
	API  string `json:"api,omitempty"`
}

// HostsOf describes the environment cfg points at, for a key minted or stored under it.
func HostsOf(cfg *config.Config) Hosts {
	h := Hosts{OTLP: cfg.OTLPURL, API: cfg.APIURL}.normalized()
	if e, err := config.EndpointsFor(cfg.Environment); err == nil && e.OTLPURL == h.OTLP && e.APIURL == h.API {
		h.Env = cfg.Environment
	}
	return h
}

func (h Hosts) normalized() Hosts {
	return Hosts{Env: h.Env, OTLP: strings.TrimRight(h.OTLP, "/"), API: strings.TrimRight(h.API, "/")}
}

func (h Hosts) resolved() Hosts {
	if h.Env == "" {
		return h
	}
	e, err := config.EndpointsFor(h.Env)
	if err != nil {
		return h
	}
	return Hosts{Env: h.Env, OTLP: e.OTLPURL, API: e.APIURL}
}

// recordHosts files a project's hosts with its key; a key already on file keeps the hosts
// it came with, so a profile pointed elsewhere cannot re-label it.
func (f *file) recordHosts(dir, projectID, key string, hosts Hosts) {
	hosts = hosts.normalized()
	if hosts == (Hosts{}) {
		return
	}
	if _, known := f.Hosts[projectID]; known {
		if old, err := f.read(dir, f.Keys, projectID, projectItem(projectID)); old == key || secret.IsUnavailable(err) {
			return
		}
	}
	f.Hosts[projectID] = hosts
}

func projectItem(projectID string) string { return "key/" + projectID }

func harnessItem(harness, projectID string) string { return "key/" + harness + "/" + projectID }

// read returns the key entries hold for id: from the file, else the keychain item; "" when
// there is none, and an error only when the keychain cannot be reached.
func (f *file) read(dir string, entries map[string]string, id, item string) (string, error) {
	v, ok := entries[id]
	if !ok {
		return "", nil
	}
	if v == "" {
		var err error
		if v, err = secret.Get(dir, item); errors.Is(err, secret.ErrNotFound) {
			return "", nil
		} else if err != nil {
			return "", err
		}
	}
	if !serverkey.Is(v) {
		return "", nil
	}
	return v, nil
}

// write files key for id: where an earlier key of id lives, else the keychain unless
// inFile; a keychain that fails gets it written to the file. A key that lands in the file
// while the item may hold an earlier one (the keychain held it, or a write timed out and
// may yet land) marks the item stale.
func (f *file) write(dir string, entries map[string]string, id, item, key string, inFile bool) {
	old, ok := entries[id]
	if ok && old != "" {
		inFile = true
	}
	if !inFile {
		err := secret.Set(dir, item, key)
		if err == nil {
			entries[id] = ""
			f.unmark(item)
			if !ok {
				f.added = append(f.added, item)
			}
			return
		}
		if secret.MayLand(err) {
			f.mark(item)
		}
	}
	if ok && old == "" {
		f.mark(item)
	}
	entries[id] = key
}

func path(dir string) string { return filepath.Join(dir, fileName) }

func load(dir string) (*file, error) {
	data, err := os.ReadFile(path(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return &file{Keys: map[string]string{}, HarnessKeys: map[string]map[string]string{}, Hosts: map[string]Hosts{}}, nil
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
	if f.Hosts == nil {
		f.Hosts = map[string]Hosts{}
	}
	return &f, nil
}

// lockWait bounds the wait for another terma's update.
const lockWait = 5 * time.Second

// update is the one way the file changes: unlocked, a concurrent install's rename
// dropped the other's key.
func update(dir string, edit func(f *file, inFile bool)) error {
	inFile := config.InsecureStorage(dir)
	var added []string
	err := flock.Locked(path(dir), lockWait, func() error {
		f, err := load(dir)
		if err != nil {
			return err
		}
		edit(f, inFile)
		added = f.added
		return save(dir, f)
	})
	if err != nil {
		for _, item := range added {
			_ = secret.Delete(dir, item)
		}
		return err
	}
	sweepStale(dir)
	return nil
}

// sweepStale removes the keychain items marked stale, now that keys.json holds their keys
// on disk, and unmarks each one it removes. It runs under the file's lock, so a key
// another terma has just put back in an item is never taken.
func sweepStale(dir string) {
	if f, err := load(dir); err != nil || len(f.Stale) == 0 {
		return
	}
	_ = flock.Locked(path(dir), lockWait, func() error {
		f, err := load(dir)
		if err != nil {
			return err
		}
		var kept []string
		for _, item := range f.Stale {
			if !f.live(item) && secret.Delete(dir, item) != nil {
				kept = append(kept, item)
			}
		}
		if len(kept) == len(f.Stale) {
			return nil
		}
		f.Stale = kept
		return save(dir, f)
	})
}

func save(dir string, f *file) error {
	return config.WriteJSON(path(dir), f, 0o600)
}

// Get returns the key for a project from the keystore under the config directory dir, or
// "" when it has none. An error means the keychain could not be reached: the key may well
// exist, so callers neither mint another nor give up on it.
func Get(dir, projectID string) (string, error) {
	f, err := load(dir)
	if err != nil {
		return "", nil
	}
	return f.read(dir, f.Keys, projectID, projectItem(projectID))
}

// Set records a project's key and the hosts of the environment it was minted in.
func Set(dir, projectID, key string, hosts Hosts) error {
	if projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a team id and a server key are required")
	}
	return update(dir, func(f *file, inFile bool) {
		f.recordHosts(dir, projectID, key, hosts)
		f.write(dir, f.Keys, projectID, projectItem(projectID), key, inFile)
	})
}

// HostsFor returns the hosts recorded with a project's key, and whether any were.
func HostsFor(dir, projectID string) (Hosts, bool) {
	f, err := load(dir)
	if err != nil {
		return Hosts{}, false
	}
	h, ok := f.Hosts[projectID]
	return h.resolved(), ok
}

// CollectionProjects includes teams whose only saved key belongs to an agent.
func CollectionProjects(dir string) []string {
	f, err := load(dir)
	if err != nil {
		return nil
	}
	ids := map[string]bool{}
	for id := range f.Keys {
		ids[id] = true
	}
	for _, keys := range f.HarnessKeys {
		for id := range keys {
			ids[id] = true
		}
	}
	var out []string
	for id := range ids {
		out = append(out, id)
	}
	return out
}

// Mask shows the key's prefix only, for status output.
func Mask(key string) string { return serverkey.Mask(key) }

// GetFor returns the key a harness last exported with for a project, or "" when it has
// none; an error means the keychain could not be reached, as for Get.
func GetFor(dir, harness, projectID string) (string, error) {
	f, err := load(dir)
	if err != nil {
		return "", nil
	}
	return f.read(dir, f.HarnessKeys[harness], projectID, harnessItem(harness, projectID))
}

// SetFor records a harness's key for a project, which is also the project's spool key.
func SetFor(dir, harness, projectID, key string, hosts Hosts) error {
	if harness == "" || projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a harness, a team id and a server key are required")
	}
	return update(dir, func(f *file, inFile bool) {
		if f.HarnessKeys[harness] == nil {
			f.HarnessKeys[harness] = map[string]string{}
		}
		f.write(dir, f.HarnessKeys[harness], projectID, harnessItem(harness, projectID), key, inFile)
		f.recordHosts(dir, projectID, key, hosts)
		f.write(dir, f.Keys, projectID, projectItem(projectID), key, inFile)
	})
}

// DeleteHarnessKeys forgets every harness's key for a project, so each harness exports with
// the project's own key; their keychain items are marked stale, for sweepStale to remove
// once the file is on disk.
func DeleteHarnessKeys(dir, projectID string) error {
	return update(dir, func(f *file, _ bool) {
		for harness, keys := range f.HarnessKeys {
			if v, ok := keys[projectID]; ok && v == "" {
				f.mark(harnessItem(harness, projectID))
			}
			delete(keys, projectID)
			if len(keys) == 0 {
				delete(f.HarnessKeys, harness)
			}
		}
	})
}

// Relocate moves every key to where config.json now keeps secrets: into the keychain, or,
// under InsecureStorage, into keys.json. A key the keychain will not take or give up
// stays where it is; with no keys.json there is nothing to move.
func Relocate(dir string) error {
	if _, err := os.Stat(path(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return update(dir, func(f *file, inFile bool) {
		f.relocate(dir, f.Keys, projectItem, inFile)
		for harness, keys := range f.HarnessKeys {
			f.relocate(dir, keys, func(id string) string { return harnessItem(harness, id) }, inFile)
		}
	})
}

// relocate moves entries' keys into the keychain, or into the file under inFile, where
// the item is marked stale for sweepStale to remove once the file is on disk.
func (f *file) relocate(dir string, entries map[string]string, item func(id string) string, inFile bool) {
	for id, v := range entries {
		switch {
		case !inFile && v != "":
			if err := secret.Set(dir, item(id), v); err == nil {
				entries[id] = ""
				f.unmark(item(id))
				f.added = append(f.added, item(id))
			} else if secret.MayLand(err) {
				f.mark(item(id))
			}
		case inFile && v == "":
			if key, err := secret.Get(dir, item(id)); err == nil {
				entries[id] = key
				f.mark(item(id))
			}
		}
	}
}
