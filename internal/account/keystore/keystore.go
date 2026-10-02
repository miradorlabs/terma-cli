// Package keystore holds the project server keys the spool flushes with, one 0600 file
// keyed by project so a flush can deliver for any repository on this machine.
package keystore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

const fileName = "keys.json"

type file struct {
	Keys map[string]string `json:"keys"`
	// HarnessKeys maps harness → project → its last key, so re-pointing a harness reuses
	// its own key instead of minting another.
	HarnessKeys map[string]map[string]string `json:"harness_keys,omitempty"`
	// Hosts maps project → its key's environment, which alone accepts the key; a missing
	// entry means "not recorded".
	Hosts map[string]Hosts `json:"hosts,omitempty"`
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
func (f *file) recordHosts(projectID, key string, hosts Hosts) {
	hosts = hosts.normalized()
	if hosts == (Hosts{}) {
		return
	}
	if _, known := f.Hosts[projectID]; known && f.Keys[projectID] == key {
		return
	}
	f.Hosts[projectID] = hosts
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
func update(edit func(*file)) error {
	p, err := path()
	if err != nil {
		return err
	}
	return flock.Locked(p, lockWait, func() error {
		f, err := load()
		if err != nil {
			return err
		}
		edit(f)
		return save(f)
	})
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

// Set records a project's key and the hosts of the environment it was minted in.
func Set(projectID, key string, hosts Hosts) error {
	if projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a team id and a server key are required")
	}
	return update(func(f *file) {
		f.recordHosts(projectID, key, hosts)
		f.Keys[projectID] = key
	})
}

// HostsFor returns the hosts recorded with a project's key, and whether any were.
func HostsFor(projectID string) (Hosts, bool) {
	f, err := load()
	if err != nil {
		return Hosts{}, false
	}
	h, ok := f.Hosts[projectID]
	return h.resolved(), ok
}

// CollectionProjects includes teams whose only saved key belongs to an agent.
func CollectionProjects() []string {
	f, err := load()
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

// SetFor records a harness's key for a project, which is also the project's spool key.
func SetFor(harness, projectID, key string, hosts Hosts) error {
	if harness == "" || projectID == "" || !serverkey.Is(key) {
		return errors.New("keystore: a harness, a team id and a server key are required")
	}
	return update(func(f *file) {
		if f.HarnessKeys[harness] == nil {
			f.HarnessKeys[harness] = map[string]string{}
		}
		f.HarnessKeys[harness][projectID] = key
		f.recordHosts(projectID, key, hosts)
		f.Keys[projectID] = key
	})
}
