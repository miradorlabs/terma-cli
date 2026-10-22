package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

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
	// Installed is the release now in place of the binary, or that its package manager
	// installed, "" when none was installed.
	Installed string
	// Notice names a newer release the pass did not install.
	Notice string
	// Replaced means another install put a release in place while this process ran, so it
	// did nothing: the next command runs that release.
	Replaced bool
	// Err is a failed install; the next attempt waits AttemptInterval, or retryAfter for a
	// release its package manager does not offer yet, or found busy (Attempt.Retries).
	Err error
}

// Auto is the relay's pass: it looks for a newer release on every call, a cheap probe of the
// latest tag (latestTag) unless that moved, as the relay spaces its calls itself, and so a
// patch release, which installs without a soak, reaches a machine within one of the relay's
// waits; a failed look still backs off (retryAfter). It installs the release for the terma
// at exe, unless the developer turned automatic updates off in configDir: in place of a
// release binary that updates itself, and through its package manager for a Homebrew or
// global npm install, when this client runs package managers (RunsManagers); the check's
// records go in stateDir. A new minor or major version waits out SoakTime first, and is
// looked up again before it is installed in place, so a release pulled since the check is
// not; a package manager needs no more than this pass's look. Each release is attempted once
// per AttemptInterval at most, so a failing install is not retried at every pass; a release
// still being built and signed (ErrNotReady) is no attempt, and each look asks after its
// files again (look), and one a package manager does not offer yet, or that found it busy,
// is tried again after retryAfter (Attempt). A process that installed a release goes on as
// it (current), so its later passes install the releases after it. progress, when set, is
// told of the download.
func (c *Client) Auto(ctx context.Context, configDir, stateDir, exe string, progress io.Writer) Outcome {
	return c.auto(ctx, 0, configDir, stateDir, exe, progress)
}

const (
	// installTimeout bounds an install in place, which is one download.
	installTimeout = 2 * time.Minute
	// tapRefreshes is how many tries at a release have Homebrew update its taps first
	// (UpgradeRequest.RefreshTaps): the tap is published minutes after the release, and one
	// whose publishing failed is left to Homebrew's own daily update, not one each hour.
	tapRefreshes = 3
	// upgradeLogFile, in the update folder, holds the last package manager upgrade's output.
	upgradeLogFile = "upgrade.log"
)

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
	install := InstallOf(exe, current)
	if cache.landed(current) {
		// An earlier relay stopped while its package manager installed this release, which it
		// was left to finish: an update like any other, said once.
		cache.end(nil, false)
		SaveCache(stateDir, cache)
		saveUpdated(stateDir, Updated{At: time.Now(), From: cache.AttemptedBy, To: current, Through: install.Manager.Name})
	}
	if !p.Auto || c.Binary == nil || cache.Unbuilt || !Newer(current, cache.Latest) || !c.installs(install) ||
		cache.held(cache.Latest, c.Version, time.Now()) || Soaking(current, cache.Latest, cache.Published, time.Now()) {
		return Outcome{Notice: notice(cache, current)}
	}
	managed := install.Method != semconv.TermaInstallMethodScript
	timeout := installTimeout
	if managed {
		timeout = UpgradeTimeout
	}
	updateCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if rel == nil {
		switch {
		case cache.Failed:
			// A check that failed waits out its backoff before any lookup, this one included.
			return Outcome{Notice: notice(cache, current)}
		case managed:
			// This pass's look found the tag unmoved (latestTag), and a package manager needs
			// none of the release's files: the API, which limits how often it is asked, is not
			// asked again, on a retry least of all.
			rel = &Release{TagName: "v" + cache.Latest, PublishedAt: cache.Published}
		default:
			if rel, err = c.Latest(updateCtx); err != nil {
				// A failed lookup, retried as a failed check is, not held back a day as an
				// install; one its process was stopped during is neither.
				if ctx.Err() == nil {
					cache.CheckedAt, cache.Failed, cache.Failures = time.Now(), true, cache.Failures+1
					SaveCache(stateDir, cache)
				}
				return Outcome{Err: err}
			}
			// The release may have been pulled, or followed by another, since the check.
			cache.Found(rel)
		}
	}
	// One that followed may be without its files still, or not be one to install by itself.
	if cache.Unbuilt || !Newer(current, rel.TagName) || Soaking(current, rel.Version(), rel.PublishedAt, time.Now()) {
		SaveCache(stateDir, cache)
		return Outcome{Notice: notice(cache, current)}
	}
	target := rel.Version()
	cache.begin(target, c.Version, time.Now())
	SaveCache(stateDir, cache)
	if progress != nil {
		fmt.Fprintf(progress, "Updating terma %s → %s…\n", current, target)
	}
	installed, err := c.install(updateCtx, install, rel, UpgradeRequest{From: current, To: target,
		Log: statePath(stateDir, upgradeLogFile), RefreshTaps: cache.Retries < tapRefreshes}, progress)
	if err == nil && installed != target {
		// A tap a release or more behind installed one between: it is in place, and the
		// release asked for is tried again soon.
		cache.end(fmt.Errorf("%s installed terma %s, not %s yet: %w", install.Manager.Name, installed, target, ErrNotOffered), false)
	} else {
		cache.end(err, err != nil && ctx.Err() != nil)
	}
	SaveCache(stateDir, cache)
	if errors.Is(err, ErrReplaced) {
		return Outcome{Replaced: true}
	}
	if err != nil {
		return Outcome{Err: err}
	}
	// Recorded as the new version's check, so its first pass does not look again; one a
	// lagging tap installed in place of the latest still names the latest.
	cache.Current, cache.CheckedAt = installed, time.Now()
	if installed == target {
		cache.Latest, cache.Published = installed, rel.PublishedAt
	}
	SaveCache(stateDir, cache)
	// A pass that reports its progress has said so already.
	saveUpdated(stateDir, Updated{At: time.Now(), From: current, To: installed, Through: install.Manager.Name, Told: progress != nil})
	return Outcome{Installed: installed}
}

// installs reports whether this client installs new releases for install by itself: one
// terma can update (Install.Automatic does not say unsupported), and for a package manager's
// install only a client that runs package managers (RunsManagers), as the relay's does.
func (c *Client) installs(i Install) bool {
	return i.updatable() && (i.Method == semconv.TermaInstallMethodScript || c.RunsManagers)
}

// install puts rel in place for i: in place of its binary, or through its package manager
// as r asks, the process going on as the release at the link the manager moved to it.
func (c *Client) install(ctx context.Context, i Install, rel *Release, r UpgradeRequest, progress io.Writer) (string, error) {
	if i.Method == semconv.TermaInstallMethodScript {
		return c.Apply(ctx, rel, i.Exe, progress)
	}
	upgrade := c.upgrade
	if upgrade == nil {
		upgrade = func(ctx context.Context, m Manager, r UpgradeRequest) (string, error) { return m.Upgrade(ctx, r) }
	}
	installed, err := upgrade(ctx, i.Manager, r)
	if err != nil {
		return "", err
	}
	c.adopt(installed, i.Manager.Terma)
	return installed, nil
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
