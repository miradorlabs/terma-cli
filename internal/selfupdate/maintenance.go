package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Preferences applies to this installation, independently of account profiles.
type Preferences struct {
	Auto bool `json:"auto_update"`
}

// LoadPreferences reads the update choice from config.json under the config directory dir;
// it defaults to notifications, without automatic replacement.
func LoadPreferences(dir string) (Preferences, error) {
	file, err := config.LoadFile(dir)
	if err != nil {
		return Preferences{}, err
	}
	return Preferences{Auto: file.AutoUpdate}, nil
}

// SavePreferences records the update choice in config.json under the config directory dir.
func SavePreferences(dir string, p Preferences) error {
	return config.UpdateFile(dir, func(f *config.File) { f.AutoUpdate = p.Auto })
}

// Cache records successful checks and failed attempts, avoiding repeated offline waits.
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Failed    bool      `json:"failed,omitempty"`
	Latest    string    `json:"latest"`
	Current   string    `json:"current,omitempty"`
	AttemptAt time.Time `json:"attempt_at"`
}

// Dir is the state directory's folder for the update check, the last refresh and their lock.
const Dir = "update"

const (
	cacheFile     = "check.json"
	refreshedFile = "refreshed.json"
	lockFile      = "run.lock"
)

// statePath is name in the update folder under the state directory dir.
func statePath(dir, name string) string { return filepath.Join(dir, Dir, name) }

// LoadCache reads the last check from dir.
func LoadCache(dir string) Cache {
	var c Cache
	data, err := os.ReadFile(statePath(dir, cacheFile))
	if err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

// SaveCache records a check, creating the state directory when necessary.
func SaveCache(dir string, c Cache) {
	_ = config.WriteJSON(statePath(dir, cacheFile), c, 0600)
}

// refreshed names the release that last rewrote this machine's installed files.
type refreshed struct {
	Version string `json:"version"`
}

// NeedsRefresh reports whether version is a release newer than the last refresh's; only
// upward, so two builds on PATH never take turns rewriting files.
func NeedsRefresh(dir, version string) bool {
	if !IsRelease(version) {
		return false
	}
	var last refreshed
	if data, err := os.ReadFile(statePath(dir, refreshedFile)); err == nil {
		_ = json.Unmarshal(data, &last)
	}
	return !IsRelease(last.Version) || Newer(last.Version, version)
}

// SaveRefreshed records version, when it is a release, as the last to refresh this machine.
func SaveRefreshed(dir, version string) error {
	if !IsRelease(version) {
		return nil
	}
	return config.WriteJSON(statePath(dir, refreshedFile), refreshed{Version: version}, 0600)
}

// Lock serializes update checks and replacements, failing at once when another holds it.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o700); err != nil {
		return nil, err
	}
	return flock.TryLock(statePath(dir, lockFile))
}

func (c *Client) cachedCheck(ctx context.Context, dir, current string) (Cache, *Release) {
	cache := LoadCache(dir)
	var release *Release
	if cache.Current != "" && cache.Current != current {
		cache = Cache{AttemptAt: cache.AttemptAt}
	}
	interval := CheckInterval
	if cache.Failed {
		interval = RetryInterval
	}
	if time.Since(cache.CheckedAt) >= interval || cache.CheckedAt.After(time.Now()) {
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cache.CheckedAt, cache.Current = time.Now(), current
		rel, err := c.Latest(checkCtx)
		cache.Failed = err != nil
		if err == nil {
			cache.Latest, release = rel.Version(), rel
		}
		SaveCache(dir, cache)
	}
	return cache, release
}

func notice(cache Cache, current string) string {
	if !Newer(current, cache.Latest) {
		return ""
	}
	return fmt.Sprintf("A newer terma is available (%s → %s). Run `terma update`.", current, cache.Latest)
}

// Maintain checks for updates after a human-facing command, replacing the binary only
// for a release build with an opt-in saved in configDir; the check's records go in stateDir.
// Errors never change the command's result.
func (c *Client) Maintain(ctx context.Context, configDir, stateDir, exe string, out io.Writer) {
	if !IsRelease(c.Version) {
		return
	}
	p, err := LoadPreferences(configDir)
	if err != nil {
		return
	}
	unlock, err := Lock(stateDir)
	if err != nil {
		return
	}
	defer unlock()
	cache, rel := c.cachedCheck(ctx, stateDir, c.Version)
	if !p.Auto || !IsRelease(c.Version) || !Newer(c.Version, cache.Latest) || ManagedCommand(exe) != "" || runtime.GOOS == "windows" {
		if msg := notice(cache, c.Version); msg != "" {
			fmt.Fprintln(out, msg)
		}
		return
	}
	if time.Since(cache.AttemptAt) < CheckInterval {
		if msg := notice(cache, c.Version); msg != "" {
			fmt.Fprintln(out, msg)
		}
		return
	}
	cache.AttemptAt = time.Now()
	SaveCache(stateDir, cache)
	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if rel == nil {
		rel, err = c.Latest(updateCtx)
	}
	if err == nil && !Newer(c.Version, rel.TagName) {
		return
	}
	if err == nil {
		fmt.Fprintf(out, "Updating terma %s → %s…\n", c.Version, rel.Version())
		_, err = c.Apply(updateCtx, rel, exe, out)
	}
	if err != nil {
		fmt.Fprintf(out, "Automatic update failed: %v. Run `terma update` to retry.\n", err)
		return
	}
	cache.Latest, cache.Current, cache.CheckedAt = rel.Version(), c.Version, time.Now()
	SaveCache(stateDir, cache)
	fmt.Fprintf(out, "Updated terma to %s; the next invocation uses it.\n", rel.Version())
}
