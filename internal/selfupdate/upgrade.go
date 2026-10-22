package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// UpgradeTimeout bounds a package manager's upgrade of terma: Homebrew may update its taps
// first, which takes minutes.
const UpgradeTimeout = 15 * time.Minute

// probeTimeout bounds asking npm's registry for a release, and the upgraded terma its version.
const probeTimeout = time.Minute

// ErrNotOffered means a package manager does not offer a published release yet: Homebrew's
// tap and npm's registry are published after the release itself, npm's by a job of its own
// that may wait. It is no failed install, held back a day, but tried again soon
// (Cache.Unoffered).
var ErrNotOffered = errors.New("its package manager does not offer it yet; terma asks again later")

// Upgrade has the package manager install release version of terma in the background, as
// the relay does, and returns the version the terma at Terma then reports. Nothing waits on
// a terminal: the manager's input is empty, it is never asked to prompt, and what it prints
// goes to out, which should be a file it can go on writing to should this process stop
// first. It runs for UpgradeTimeout at most; ctx ending as the relay stops leaves it to
// finish by itself, in a session of its own, since stopping a package manager halfway can
// leave terma uninstalled.
//
// Success is the upgraded binary's version, never the manager's exit status: a manager
// that exits 0 without it (a tap or registry not updated yet) is ErrNotOffered, and one
// that fails, Homebrew refusing an untrusted tap or npm a prefix it may not write
// (EACCES), is a failed install unless the new version is in place regardless.
//
// Homebrew updates its taps before an upgrade only once a day by default
// (HOMEBREW_AUTO_UPDATE_SECS), so the upgrade has it update them first: Upgrade is called
// only once terma knows the release is out. It upgrades terma alone, without the cleanup
// that would prune the developer's other packages or upgrades of their dependents; a
// developer's HOMEBREW_NO_AUTO_UPDATE, and Homebrew's own brew.env files, still apply. npm
// is asked first whether its registry has the release, and then installs exactly that
// version, so a lagging registry never has npm install the same release again.
func (m Manager) Upgrade(ctx context.Context, version string, out io.Writer) (string, error) {
	if len(m.Argv) == 0 || m.Project != "" {
		return "", fmt.Errorf("terma cannot run %s for this installation; run `%s`", m.Name, m.Command)
	}
	env := m.backgroundEnv(os.Environ())
	argv := m.Argv
	if m.Name == "npm" {
		argv = append(slices.Clone(m.Argv[:len(m.Argv)-1]), npmPackage+"@"+version)
		// npm answers a version its registry does not have with E404, as it does a registry
		// it cannot reach: both are asked again soon.
		offered, err := probe(ctx, env, m.Argv[0], "view", npmPackage+"@"+version, "version")
		if err != nil {
			return "", fmt.Errorf("npm view found no terma %s (%w): %w", version, err, ErrNotOffered)
		}
		if offered != version {
			return "", fmt.Errorf("npm view found no terma %s: %w", version, ErrNotOffered)
		}
	}
	fmt.Fprintf(out, "%s terma: upgrading to %s with %s: %s\n", time.Now().UTC().Format(time.RFC3339), version, m.Name, strings.Join(argv, " "))
	ran := RunDetached(ctx, out, env, argv...)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	got, err := InstalledVersion(ctx, m.Terma)
	switch {
	case err == nil && (got == version || Newer(version, got)):
		return got, nil
	case ran != nil:
		return "", fmt.Errorf("%s could not upgrade terma to %s (%w); run `%s` to see why", m.Name, version, ran, m.Command)
	case err != nil:
		return "", fmt.Errorf("%s upgraded terma, but %s does not run: %w", m.Name, m.Terma, err)
	}
	return "", fmt.Errorf("%s left terma at %s, not %s: %w", m.Name, got, version, ErrNotOffered)
}

// backgroundEnv is environ for the package manager, run with no one at a terminal.
func (m Manager) backgroundEnv(environ []string) []string {
	switch m.Name {
	case "Homebrew":
		return append(environ,
			// Ask mode is Homebrew's default, though without a terminal it only prints its plan.
			"HOMEBREW_NO_ASK=1",
			// Update the taps first, whenever they were last updated.
			"HOMEBREW_AUTO_UPDATE_SECS=0",
			// Leave the developer's other packages alone.
			"HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1",
			"HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_UPDATE_REPORT_NEW=1")
	case "npm":
		// npm and the package's install script run on node, which a service's minimal PATH
		// lacks: it sits beside the npm that owns this terma, or in its prefix's bin.
		path := filepath.Dir(m.Argv[0]) + string(os.PathListSeparator) + filepath.Join(m.prefix, "bin")
		for _, kv := range environ {
			if old, ok := strings.CutPrefix(kv, "PATH="); ok && old != "" {
				path += string(os.PathListSeparator) + old
			}
		}
		return append(environ, "PATH="+path,
			"npm_config_update_notifier=false", "npm_config_fund=false", "npm_config_audit=false")
	}
	return environ
}

// RunDetached runs argv with environment env, its input empty and its output to out, in a
// session of its own, so that a service manager stopping this process does not stop it too:
// an update step the relay runs. At ctx's deadline it is killed; when ctx is cancelled it is
// left to finish by itself, and out should be a file it can go on writing to.
func RunDetached(ctx context.Context, out io.Writer, env []string, argv ...string) error {
	c := exec.Command(argv[0], argv[1:]...)
	c.Env, c.Stdout, c.Stderr = env, out, out
	// Through a pipe, which out is when it is no file, what it started may hold it open.
	c.WaitDelay = time.Second
	procinfo.Detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_ = c.Process.Kill()
			<-done
		}
		return ctx.Err()
	}
}

// InstalledVersion is the version the terma at path reports (`terma version`), with no
// terminal, within probeTimeout.
func InstalledVersion(ctx context.Context, path string) (string, error) {
	got, err := probe(ctx, os.Environ(), path, "version")
	if err != nil {
		return "", err
	}
	name, version, _ := strings.Cut(got, " ")
	version, _, _ = strings.Cut(version, " ")
	if name != BinaryName || !IsRelease(version) {
		return "", fmt.Errorf("%s version printed %q", path, got)
	}
	return version, nil
}

// probe is what argv prints, trimmed, run with environment env and no input, within
// probeTimeout.
func probe(ctx context.Context, env []string, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env, c.Stdout, c.Stderr = env, &stdout, &stderr
	c.WaitDelay = time.Second
	if err := c.Run(); err != nil {
		// Its first line says why: npm's error code, a shell's missing node.
		if why, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); why != "" {
			return "", fmt.Errorf("%w: %s", err, why)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
