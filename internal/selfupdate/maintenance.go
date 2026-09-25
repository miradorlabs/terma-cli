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

// LoadPreferences defaults to notifications, without automatic replacement.
func LoadPreferences(dir string) (Preferences, error) {
	var p Preferences
	data, err := os.ReadFile(filepath.Join(dir, "updates.json"))
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(data, &p)
	return p, err
}

// SavePreferences persists the user's update choice atomically.
func SavePreferences(dir string, p Preferences) error {
	return config.WriteJSON(filepath.Join(dir, "updates.json"), p, 0600)
}

// Cache records successful checks and failed attempts, avoiding repeated offline waits.
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Failed    bool      `json:"failed,omitempty"`
	Latest    string    `json:"latest"`
	Current   string    `json:"current,omitempty"`
	AttemptAt time.Time `json:"attempt_at,omitempty"`
}

const cacheFile = "update-check.json"

// LoadCache reads the last check from dir.
func LoadCache(dir string) Cache {
	var c Cache
	data, err := os.ReadFile(filepath.Join(dir, cacheFile))
	if err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

// SaveCache records a check, creating the state directory when necessary.
func SaveCache(dir string, c Cache) {
	_ = config.WriteJSON(filepath.Join(dir, cacheFile), c, 0600)
}

const refreshedFile = "refreshed.json"

// refreshed names the release that last rewrote this machine's installed files.
type refreshed struct {
	Version string `json:"version"`
}

// NeedsRefresh reports whether version is a release newer than the one that last
// refreshed the files terma installed on this machine. Only upward: two builds side by
// side on PATH must not take turns rewriting each other's files.
func NeedsRefresh(dir, version string) bool {
	if !IsRelease(version) {
		return false
	}
	var last refreshed
	if data, err := os.ReadFile(filepath.Join(dir, refreshedFile)); err == nil {
		_ = json.Unmarshal(data, &last)
	}
	return !IsRelease(last.Version) || Newer(last.Version, version)
}

// SaveRefreshed records version as the one that last refreshed this machine. A build
// that is not a release records nothing.
func SaveRefreshed(dir, version string) error {
	if !IsRelease(version) {
		return nil
	}
	return config.WriteJSON(filepath.Join(dir, refreshedFile), refreshed{Version: version}, 0600)
}

// Lock serializes checks and replacements across simultaneous CLI processes.
// It returns immediately when another update is already running.
func Lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return flock.TryLock(filepath.Join(dir, "update.lock"))
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

// Notice returns an available-update message. Successful lookups are cached for
// a day; failed lookups retry after 15 minutes.
func (c *Client) Notice(ctx context.Context, dir, current string) string {
	if !IsRelease(current) {
		return ""
	}
	cache, _ := c.cachedCheck(ctx, dir, current)
	return notice(cache, current)
}

// notice names `terma update` for every installation: it upgrades a package-managed one
// through its package manager.
func notice(cache Cache, current string) string {
	if !Newer(current, cache.Latest) {
		return ""
	}
	return fmt.Sprintf("A newer terma is available (%s → %s). Run `terma update`.", current, cache.Latest)
}

// Maintain checks for updates after a human-facing command. Errors never change the
// command's result. Automatic replacement requires a saved opt-in and a release build.
func (c *Client) Maintain(ctx context.Context, dir, exe string, out io.Writer) {
	if !IsRelease(c.Version) {
		return
	}
	p, err := LoadPreferences(dir)
	if err != nil {
		return
	}
	unlock, err := Lock(dir)
	if err != nil {
		return
	}
	defer unlock()
	cache, rel := c.cachedCheck(ctx, dir, c.Version)
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
	SaveCache(dir, cache)
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
	SaveCache(dir, cache)
	fmt.Fprintf(out, "Updated terma to %s; the next invocation uses it.\n", rel.Version())
}
