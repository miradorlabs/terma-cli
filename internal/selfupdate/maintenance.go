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
}

// held reports whether latest is a release whose install failed less than AttemptInterval
// ago, tried by this build (running) or a later one, so that it is not tried again yet.
func (c Cache) held(running string) bool {
	return c.Attempted == c.Latest && time.Since(c.AttemptAt) < AttemptInterval && !Newer(c.AttemptedBy, running)
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
		cache = Cache{AttemptAt: cache.AttemptAt, Attempted: cache.Attempted, AttemptedBy: cache.AttemptedBy}
	}
	interval := every
	if cache.Failed {
		interval = retryAfter(cache.Failures)
	}
	if time.Since(cache.CheckedAt) >= interval || cache.CheckedAt.After(time.Now()) {
		var err error
		release, err = c.look(ctx, &cache)
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
	_, _, _, pick := PickAsset(release, runtime.GOOS, runtime.GOARCH)
	cache.Latest, cache.Published, cache.Unbuilt = release.Version(), release.PublishedAt, errors.Is(pick, ErrNotReady)
	return release, nil
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
// from or installed (Binary): another install has put a release in place since, so this
// process's version no longer says what is installed, and it must not replace it. False
// without Binary, which cannot tell. The file's size and time are compared too: a file this
// process installed is not held open as the one it started from is, so once another install
// replaces it, a later file may be given its inode.
func (c *Client) Replaced(exe string) bool {
	if c.Binary == nil {
		return false
	}
	now, err := os.Stat(exe)
	return err != nil || !os.SameFile(now, c.Binary) || now.Size() != c.Binary.Size() || !now.ModTime().Equal(c.Binary.ModTime())
}

// current is the release installed where this process started: its own, or the one it has
// installed in its place since. A process that keeps running after it installed a release,
// as the relay does until its agents pause, compares the next release to that one.
func (c *Client) current() string {
	if c.installed != "" {
		return c.installed
	}
	return c.Version
}

// adopt has this process go on as version, which it just installed at exe: what is
// installed is no longer what it started from but is its own doing, so the next release is
// installed over it rather than refused as another install's (Replaced).
func (c *Client) adopt(version, exe string) {
	if info, err := os.Stat(exe); err == nil {
		c.installed, c.Binary = version, info
	}
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

// Auto is the relay's pass: it looks for a newer release on every call, a cheap probe of the
// latest tag (latestTag) unless that moved, as the relay spaces its calls itself, and so a
// patch release, which installs without a soak, reaches a machine within one of the relay's
// waits; a failed look still backs off (retryAfter). It installs the release in place of exe
// for a release build that updates itself, unless the developer turned automatic updates
// off in configDir; the check's records go in stateDir. A new minor or major version waits
// out SoakTime first, and is looked up again before it is installed, so a release pulled
// since the check is not. Each release is attempted once per AttemptInterval at most, so a
// failing install is not retried at every pass; a release still being built and signed
// (ErrNotReady) is no attempt, and each look asks after its files again (look). A process
// that installed a release goes on as it (current), so its later passes install the releases
// after it. progress, when set, is told of the download.
func (c *Client) Auto(ctx context.Context, configDir, stateDir, exe string, progress io.Writer) Outcome {
	return c.auto(ctx, 0, configDir, stateDir, exe, progress)
}

// auto is Auto with every as the least time between looks.
func (c *Client) auto(ctx context.Context, every time.Duration, configDir, stateDir, exe string, progress io.Writer) Outcome {
	current := c.current()
	if !IsRelease(current) {
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
	cache, rel := c.cachedCheck(ctx, stateDir, current, every)
	// The latest release has no files yet: nothing to install, and nothing to say, as `terma
	// update` could not install it either.
	if cache.Unbuilt {
		return Outcome{}
	}
	if !p.Auto || c.Binary == nil || !Newer(current, cache.Latest) || !UpdatesItself(exe) || cache.held(c.Version) ||
		Soaking(current, cache.Latest, cache.Published, time.Now()) {
		return Outcome{Notice: notice(cache, current)}
	}
	updateCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if rel == nil {
		// A check that failed waits out its backoff before any lookup, this one included.
		if cache.Failed {
			return Outcome{Notice: notice(cache, current)}
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
	if !Newer(current, rel.TagName) || Soaking(current, rel.Version(), rel.PublishedAt, time.Now()) {
		SaveCache(stateDir, cache)
		return Outcome{Notice: notice(cache, current)}
	}
	// The one that followed may be without its files still: the next look asks after them.
	if _, _, _, err := PickAsset(rel, runtime.GOOS, runtime.GOARCH); errors.Is(err, ErrNotReady) {
		cache.Unbuilt = true
		SaveCache(stateDir, cache)
		return Outcome{}
	}
	cache.AttemptAt, cache.Attempted, cache.AttemptedBy = time.Now(), rel.Version(), c.Version
	SaveCache(stateDir, cache)
	if progress != nil {
		fmt.Fprintf(progress, "Updating terma %s → %s…\n", current, rel.Version())
	}
	_, err = c.Apply(updateCtx, rel, exe, progress)
	replaced := errors.Is(err, ErrReplaced)
	if replaced || err != nil && ctx.Err() != nil {
		// Not the release's failure, so it is not held back: another install's binary is in
		// place, whose own process may try at once, or this process was stopped during the
		// install, as a relay is that restarts on the release before.
		cache.AttemptAt, cache.Attempted, cache.AttemptedBy = time.Time{}, "", ""
		SaveCache(stateDir, cache)
	}
	if replaced {
		return Outcome{Replaced: true}
	}
	if err != nil {
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
