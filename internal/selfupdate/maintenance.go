package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Preferences applies to this installation, independently of account profiles.
type Preferences struct {
	Auto bool `json:"auto_update"`
}

// LoadPreferences reads the update choice from config.json under the config directory dir;
// it defaults to automatic updates.
func LoadPreferences(dir string) (Preferences, error) {
	file, err := config.LoadFile(dir)
	if err != nil {
		return Preferences{}, err
	}
	return Preferences{Auto: file.AutoUpdate == nil || *file.AutoUpdate}, nil
}

// SavePreferences records the update choice in config.json under the config directory dir;
// on, the default, is recorded as no choice at all.
func SavePreferences(dir string, p Preferences) error {
	return config.UpdateFile(dir, func(f *config.File) {
		f.AutoUpdate = nil
		if !p.Auto {
			f.AutoUpdate = &p.Auto
		}
	})
}

// Cache records successful checks and failed attempts, avoiding repeated offline waits.
type Cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Failed    bool      `json:"failed,omitempty"`
	// Failures counts the lookups that have failed in a row, which space out the next.
	Failures int    `json:"failures,omitempty"`
	Latest   string `json:"latest"`
	// Published is when Latest was published, for its soak; zero in a check recorded before.
	Published time.Time `json:"published,omitzero"`
	// Unbuilt means Latest was last seen without its files (ErrNotReady): each look asks
	// after them again, and until they are there it is neither installed nor announced.
	Unbuilt   bool      `json:"unbuilt,omitempty"`
	Current   string    `json:"current,omitempty"`
	AttemptAt time.Time `json:"attempt_at"`
	// Attempted is the release AttemptAt tried to install: another may be tried at once.
	Attempted string `json:"attempted,omitempty"`
	// AttemptedBy is the build that tried it, "" for one from before this was recorded. A
	// later build is not held back by it (held): the failure may have been the earlier
	// build's own, such as a release key it lacks or a fault since fixed.
	AttemptedBy string `json:"attempted_by,omitempty"`
	// Unoffered counts the attempts in a row whose package manager ran but did not have
	// Attempted yet (ErrNotOffered): its tap or registry is published after the release, so
	// it is tried again after retryAfter rather than AttemptInterval.
	Unoffered int `json:"unoffered,omitempty"`
	// Failure is why the install of Attempted failed, or its package manager did not offer
	// it, for doctor; "" when neither.
	Failure string `json:"failure,omitempty"`
}

// Found records rel as the latest release, with whether its files are there yet (Unbuilt):
// every look that fetches a release records it so, `terma update`'s as much as the relay's.
func (c *Cache) Found(rel *Release) {
	_, _, _, err := PickAsset(rel, runtime.GOOS, runtime.GOARCH)
	c.Latest, c.Published, c.Unbuilt = rel.Version(), rel.PublishedAt, errors.Is(err, ErrNotReady)
}

// attempt is the record of the last install tried alone, which outlives what a look found.
func (c Cache) attempt() Cache {
	return Cache{AttemptAt: c.AttemptAt, Attempted: c.Attempted, AttemptedBy: c.AttemptedBy, Unoffered: c.Unoffered, Failure: c.Failure}
}

// Checked records in dir a look that `terma update` made for itself, which found rel for a
// terma at current, and returns the record. It keeps the last install tried, as every look
// does: a release held back after a failed install stays so.
func Checked(dir, current string, rel *Release) Cache {
	cache := LoadCache(dir).attempt()
	cache.CheckedAt, cache.Current = time.Now(), current
	cache.Found(rel)
	SaveCache(dir, cache)
	return cache
}

// held reports whether latest is a release whose install failed less than AttemptInterval
// ago, tried by this build (running) or a later one, so that it is not tried again yet. One
// its package manager did not offer yet waits only retryAfter its count of such attempts.
func (c Cache) held(running string) bool {
	return c.Attempted == c.Latest && time.Now().Before(c.Retry()) && !Newer(c.AttemptedBy, running)
}

// Retry is when the release last attempted may be tried again after it failed.
func (c Cache) Retry() time.Time {
	if c.Unoffered > 0 {
		return c.AttemptAt.Add(retryAfter(c.Unoffered))
	}
	return c.AttemptAt.Add(AttemptInterval)
}

// maxRetryWait caps retryAfter: the window GitHub's rate limit resets in, so a machine back
// online after a night's failed lookups still takes a patch within the hour.
const maxRetryWait = time.Hour

// retryAfter is how long a lookup that has failed failures times in a row waits before the
// next: RetryInterval, doubling up to maxRetryWait, so a rate-limited network is not kept so.
func retryAfter(failures int) time.Duration {
	wait := RetryInterval
	for range failures - 1 {
		if wait *= 2; wait >= maxRetryWait {
			return maxRetryWait
		}
	}
	return wait
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

// web is GitHub's web base for latestTag: BaseURL when set, so one test server answers both.
func (c *Client) web() string {
	if c.BaseURL != "" {
		return c.base()
	}
	return "https://github.com"
}

// latestTag is the version of the release github.com's latest-release page redirects to, ""
// when the answer names none; an error when the page could not be asked or refused. The API
// answers unauthenticated callers 60 times an hour per address, an unchanged answer (304)
// included, which machines sharing an address and checking every few minutes would spend;
// the page is not held to that published limit, so the frequent check asks it first.
func (c *Client) latestTag(ctx context.Context) (string, error) {
	resp, err := c.head(ctx, "/releases/latest")
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("github latest release page: HTTP %d", resp.StatusCode)
	}
	_, tag, found := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	if !found || !IsRelease(tag) {
		return "", nil
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// cachedCheck looks for the latest release once every has passed since the last look (after
// failed looks, retryAfter), returning the record and the release when this call fetched it.
func (c *Client) cachedCheck(ctx context.Context, dir, current string, every time.Duration) (Cache, *Release) {
	cache := LoadCache(dir)
	var release *Release
	if cache.Current != "" && cache.Current != current {
		cache = cache.attempt()
	}
	interval := every
	if cache.Failed {
		interval = retryAfter(cache.Failures)
	}
	if time.Since(cache.CheckedAt) >= interval || cache.CheckedAt.After(time.Now()) {
		before := cache
		var err error
		release, err = c.look(ctx, &cache)
		// A look its process was stopped during is no look, failed or otherwise.
		if err != nil && ctx.Err() != nil {
			return before, nil
		}
		cache.CheckedAt, cache.Current, cache.Failed = time.Now(), current, err != nil
		if err != nil {
			cache.Failures++
		} else {
			cache.Failures = 0
		}
		SaveCache(dir, cache)
	}
	return cache, release
}

// look asks what the latest release is and records the answer in cache, returning the
// release when it had to be fetched from the API. A look whose last answer is complete asks
// the release page for the latest tag first, and of a tag that moved, or whose release had no
// files yet, whether its files are there (built): it fetches the release only once they are,
// so a release costs a machine one lookup however long its build waits. A page that refuses
// is a failed look, which backs off rather than sending every machine behind a throttled
// address on to the rate-limited API.
func (c *Client) look(ctx context.Context, cache *Cache) (*Release, error) {
	if !cache.Failed && cache.Latest != "" && (!cache.Published.IsZero() || cache.Unbuilt) {
		tag, err := c.latestTag(ctx)
		if err != nil || tag == cache.Latest && !cache.Unbuilt {
			return nil, err
		}
		if tag != "" {
			ready, err := c.built(ctx, tag)
			if err != nil {
				return nil, err
			}
			if !ready {
				if tag != cache.Latest {
					cache.Latest, cache.Published = tag, time.Time{}
				}
				cache.Unbuilt = true
				return nil, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	release, err := c.Latest(ctx)
	if err != nil {
		return nil, err
	}
	cache.Found(release)
	return release, nil
}

// notice names a newer release for the developer to install with `terma update`: not one
// without its files yet, which that could not install either.
func notice(cache Cache, current string) string {
	if cache.Unbuilt || !Newer(current, cache.Latest) {
		return ""
	}
	return fmt.Sprintf("A newer terma is available (%s → %s). Run `terma update`.", current, cache.Latest)
}
