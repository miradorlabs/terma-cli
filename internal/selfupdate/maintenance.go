package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Current   string    `json:"current,omitempty"`
	AttemptAt time.Time `json:"attempt_at"`
	// Attempted is the release AttemptAt tried to install: another may be tried at once.
	Attempted string `json:"attempted,omitempty"`
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.web()+"/"+Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgentPrefix+c.Version)
	client := c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("github latest release page: HTTP %d", resp.StatusCode)
	}
	_, tag, found := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	if !found || !IsRelease(tag) {
		return "", nil
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// cachedCheck looks the latest release up once every has passed since the last look (after
// failed looks, retryAfter), returning the record and the release when this call fetched it.
// A look whose last answer is complete probes the latest tag first and fetches the release
// only when the tag moved; a probe that fails is a failed look, which backs off rather than
// sending every machine behind a throttled address on to the rate-limited API.
func (c *Client) cachedCheck(ctx context.Context, dir, current string, every time.Duration) (Cache, *Release) {
	cache := LoadCache(dir)
	var release *Release
	if cache.Current != "" && cache.Current != current {
		cache = Cache{AttemptAt: cache.AttemptAt, Attempted: cache.Attempted}
	}
	interval := every
	if cache.Failed {
		interval = retryAfter(cache.Failures)
	}
	if time.Since(cache.CheckedAt) >= interval || cache.CheckedAt.After(time.Now()) {
		var err error
		if !cache.Failed && cache.Latest != "" && !cache.Published.IsZero() {
			var tag string
			if tag, err = c.latestTag(ctx); err == nil && tag == cache.Latest {
				cache.CheckedAt, cache.Current = time.Now(), current
				SaveCache(dir, cache)
				return cache, nil
			}
		}
		if err == nil {
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			release, err = c.Latest(checkCtx)
			cancel()
		}
		cache.CheckedAt, cache.Current, cache.Failed = time.Now(), current, err != nil
		if err != nil {
			cache.Failures++
		} else {
			cache.Latest, cache.Published, cache.Failures = release.Version(), release.PublishedAt, 0
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

// UpdatesItself reports whether the binary at exe is one terma replaces on its own: not a
// package manager's, and not on Windows, where a running binary cannot be swapped yet.
func UpdatesItself(exe string) bool {
	return ManagedCommand(exe) == "" && runtime.GOOS != "windows"
}

// Replaced reports whether the executable at exe is no longer the one this process started
// from (Binary): another install has put a release in place since, so this process's version
// no longer says what is installed, and it must not replace it. False without Binary, which
// cannot tell.
func (c *Client) Replaced(exe string) bool {
	if c.Binary == nil {
		return false
	}
	now, err := os.Stat(exe)
	return err != nil || !os.SameFile(now, c.Binary)
}

// Outcome is what one automatic update pass did.
type Outcome struct {
	// Installed is the release now in place of the binary, "" when none was installed.
	Installed string
	// Notice names a newer release the pass did not install.
	Notice string
	// Replaced means another install put a release in place while this process ran, so it
	// did nothing: the next command runs that release.
	Replaced bool
	// Err is a failed install; the next attempt waits AttemptInterval.
	Err error
}

// Auto is the relay's pass: it checks for a newer release every CheckInterval at most and
// installs it in place of exe for a release build that updates itself, unless the developer
// turned automatic updates off in configDir; the check's records go in stateDir. A new minor
// or major version waits out SoakTime first, and is looked up again before it is installed,
// so a release pulled since the check is not. Each release is attempted once per
// AttemptInterval at most, so a failing install is not retried at every pass. progress, when
// set, is told of the download.
func (c *Client) Auto(ctx context.Context, configDir, stateDir, exe string, progress io.Writer) Outcome {
	return c.auto(ctx, CheckInterval, configDir, stateDir, exe, progress)
}

// auto is Auto with every as the interval between looks.
func (c *Client) auto(ctx context.Context, every time.Duration, configDir, stateDir, exe string, progress io.Writer) Outcome {
	if !IsRelease(c.Version) {
		return Outcome{}
	}
	p, err := LoadPreferences(configDir)
	if err != nil {
		return Outcome{}
	}
	unlock, err := Lock(stateDir)
	if err != nil {
		return Outcome{}
	}
	defer unlock()
	// Checked under the lock: another install may have put a later release in place, which
	// this process's version says nothing about.
	if c.Replaced(exe) {
		return Outcome{Replaced: true}
	}
	cache, rel := c.cachedCheck(ctx, stateDir, c.Version, every)
	attempted := cache.Attempted == cache.Latest && time.Since(cache.AttemptAt) < AttemptInterval
	if !p.Auto || c.Binary == nil || !Newer(c.Version, cache.Latest) || !UpdatesItself(exe) || attempted ||
		Soaking(c.Version, cache.Latest, cache.Published, time.Now()) {
		return Outcome{Notice: notice(cache, c.Version)}
	}
	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if rel == nil {
		// A check that failed waits out its backoff before any lookup, this one included.
		if cache.Failed {
			return Outcome{Notice: notice(cache, c.Version)}
		}
		if rel, err = c.Latest(updateCtx); err != nil {
			// A failed lookup, retried as a failed check is, not held back a day as an install.
			cache.CheckedAt, cache.Failed, cache.Failures = time.Now(), true, cache.Failures+1
			SaveCache(stateDir, cache)
			return Outcome{Err: err}
		}
		// The release may have been pulled, or followed by another, since the check.
		cache.Latest, cache.Published = rel.Version(), rel.PublishedAt
	}
	if !Newer(c.Version, rel.TagName) || Soaking(c.Version, rel.Version(), rel.PublishedAt, time.Now()) {
		SaveCache(stateDir, cache)
		return Outcome{Notice: notice(cache, c.Version)}
	}
	cache.AttemptAt, cache.Attempted = time.Now(), rel.Version()
	SaveCache(stateDir, cache)
	if progress != nil {
		fmt.Fprintf(progress, "Updating terma %s → %s…\n", c.Version, rel.Version())
	}
	if _, err := c.Apply(updateCtx, rel, exe, progress); errors.Is(err, ErrReplaced) {
		// No attempt was made on the binary now in place: its own process may try at once.
		cache.AttemptAt, cache.Attempted = time.Time{}, ""
		SaveCache(stateDir, cache)
		return Outcome{Replaced: true}
	} else if err != nil {
		return Outcome{Err: err}
	}
	// Recorded as the new version's check, so its first pass does not look again.
	cache.Latest, cache.Published, cache.Current, cache.CheckedAt = rel.Version(), rel.PublishedAt, rel.Version(), time.Now()
	SaveCache(stateDir, cache)
	return Outcome{Installed: rel.Version()}
}

// Maintain is Auto after a human-facing command, looking every CommandCheckInterval at most
// and saying what it did. Errors never change the command's result.
func (c *Client) Maintain(ctx context.Context, configDir, stateDir, exe string, out io.Writer) {
	o := c.auto(ctx, CommandCheckInterval, configDir, stateDir, exe, out)
	switch {
	case o.Err != nil:
		fmt.Fprintf(out, "Automatic update failed: %v. Run `terma update` to retry.\n", o.Err)
	case o.Installed != "":
		fmt.Fprintf(out, "Updated terma to %s; the next invocation uses it.\n", o.Installed)
	case o.Replaced:
		fmt.Fprintln(out, "terma was updated while this command ran; the next invocation uses the new version.")
	case o.Notice != "":
		fmt.Fprintln(out, o.Notice)
	}
}
