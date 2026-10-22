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
	// release its package manager does not offer yet (ErrNotOffered).
	Err error
}

// Auto is the relay's pass: it looks for a newer release on every call, a cheap probe of the
// latest tag (latestTag) unless that moved, as the relay spaces its calls itself, and so a
// patch release, which installs without a soak, reaches a machine within one of the relay's
// waits; a failed look still backs off (retryAfter). It installs the release for the terma
// at exe, unless the developer turned automatic updates off in configDir: in place of a
// release binary that updates itself, and through its package manager (Upgrade) for a
// Homebrew or global npm install, when this client runs package managers; the check's
// records go in stateDir. A new minor or major version waits out SoakTime first, and is
// looked up again before it is installed, so a release pulled since the check is not. Each
// release is attempted once per AttemptInterval at most, so a failing install is not retried
// at every pass; a release still being built and signed (ErrNotReady) is no attempt, and
// each look asks after its files again (look), and one a package manager does not offer yet
// (ErrNotOffered) is tried again after retryAfter. A process that installed a release goes
// on as it (current), so its later passes install the releases after it. progress, when
// set, is told of the download.
func (c *Client) Auto(ctx context.Context, configDir, stateDir, exe string, progress io.Writer) Outcome {
	return c.auto(ctx, 0, configDir, stateDir, exe, progress)
}

// installTimeout bounds an install in place, which is one download.
const installTimeout = 2 * time.Minute

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
	if !p.Auto || c.Binary == nil || cache.Unbuilt || !Newer(current, cache.Latest) || !c.installs(install) || cache.held(c.Version) ||
		Soaking(current, cache.Latest, cache.Published, time.Now()) {
		return Outcome{Notice: notice(cache, current)}
	}
	timeout := installTimeout
	if install.Method != semconv.TermaInstallMethodScript {
		timeout = UpgradeTimeout
	}
	updateCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if rel == nil {
		// A check that failed waits out its backoff before any lookup, this one included.
		if cache.Failed {
			return Outcome{Notice: notice(cache, current)}
		}
		if rel, err = c.Latest(updateCtx); err != nil {
			// A failed lookup, retried as a failed check is, not held back a day as an install;
			// one its process was stopped during is neither.
			if ctx.Err() == nil {
				cache.CheckedAt, cache.Failed, cache.Failures = time.Now(), true, cache.Failures+1
				SaveCache(stateDir, cache)
			}
			return Outcome{Err: err}
		}
		// The release may have been pulled, or followed by another, since the check.
		cache.Found(rel)
	}
	// One that followed may be without its files still, or not be one to install by itself.
	if cache.Unbuilt || !Newer(current, rel.TagName) || Soaking(current, rel.Version(), rel.PublishedAt, time.Now()) {
		SaveCache(stateDir, cache)
		return Outcome{Notice: notice(cache, current)}
	}
	unoffered := 0
	if cache.Attempted == rel.Version() {
		unoffered = cache.Unoffered
	}
	cache.AttemptAt, cache.Attempted, cache.AttemptedBy, cache.Unoffered, cache.Failure = time.Now(), rel.Version(), c.Version, 0, ""
	SaveCache(stateDir, cache)
	if progress != nil {
		fmt.Fprintf(progress, "Updating terma %s → %s…\n", current, rel.Version())
	}
	installed, err := c.install(updateCtx, install, rel, progress)
	replaced := errors.Is(err, ErrReplaced)
	switch {
	case replaced || err != nil && ctx.Err() != nil:
		// Not the release's failure, so it is not held back: another install's binary is in
		// place, whose own process may try at once, or this process was stopped during the
		// install, as a relay is that restarts on the release before.
		cache.AttemptAt, cache.Attempted, cache.AttemptedBy, cache.Unoffered = time.Time{}, "", "", 0
		SaveCache(stateDir, cache)
	case errors.Is(err, ErrNotOffered):
		cache.Unoffered, cache.Failure = unoffered+1, err.Error()
		SaveCache(stateDir, cache)
	case err != nil:
		cache.Failure = err.Error()
		SaveCache(stateDir, cache)
	}
	if replaced {
		return Outcome{Replaced: true}
	}
	if err != nil {
		return Outcome{Err: err}
	}
	// Recorded as the new version's check, so its first pass does not look again.
	cache.Latest, cache.Published, cache.Current, cache.CheckedAt = installed, rel.PublishedAt, installed, time.Now()
	SaveCache(stateDir, cache)
	// A pass that reports its progress has said so already.
	saveUpdated(stateDir, Updated{At: time.Now(), From: current, To: installed, Through: install.Manager.Name, Told: progress != nil})
	return Outcome{Installed: installed}
}

// installs reports whether this client installs new releases for install by itself: one
// terma can update (Install.Automatic does not say unsupported), and for a package manager's
// install only a client that runs package managers (Upgrade), as the relay's does. An
// interactive command leaves those to the relay: the manager may take minutes, and the
// terminal is the developer's.
func (c *Client) installs(i Install) bool {
	return i.updatable() && (i.Method == semconv.TermaInstallMethodScript || c.Upgrade != nil)
}

// install puts rel in place for i: through its package manager, or in place of its binary.
func (c *Client) install(ctx context.Context, i Install, rel *Release, progress io.Writer) (string, error) {
	if i.Method == semconv.TermaInstallMethodScript {
		return c.Apply(ctx, rel, i.Exe, progress)
	}
	installed, err := c.Upgrade(ctx, i.Manager, rel.Version())
	if err != nil {
		return "", err
	}
	// The process goes on as the release at the link the manager moved to it.
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
