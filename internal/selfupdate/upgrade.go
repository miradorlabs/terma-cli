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
	"runtime"
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
// (Attempt.Retries).
var ErrNotOffered = errors.New("its package manager does not offer it yet; terma asks again later")

// ErrBusy means another of the package manager's processes held its lock, as the developer's
// own `brew upgrade` does while it runs: tried again soon too.
var ErrBusy = errors.New("another of its package manager's processes is running; terma asks again later")

// UpgradeRequest is one upgrade of terma through the package manager that owns it.
type UpgradeRequest struct {
	// From is the release installed now, and To the one to install.
	From, To string
	// Log is the file the manager's output goes to, a new one for each upgrade, the last
	// one's kept as <Log>.1 (openLog): a file, which the manager goes on writing to should
	// the relay stop first.
	Log string
	// RefreshTaps has Homebrew update its taps before the upgrade, which it otherwise does
	// once a day: the first few tries at a release ask for it, not every retry.
	RefreshTaps bool
}

// Upgrade has the package manager install release r.To of terma in the background, as the
// relay does, and returns the version the terma at Terma then reports. Nothing waits on a
// terminal: the manager's input is empty, it is never asked to prompt, and what it prints
// goes to r.Log. It runs for UpgradeTimeout at most, after which it is stopped with every
// process it started; ctx ending as the relay stops leaves it to finish by itself
// (RunDetached), since stopping a package manager halfway can leave terma uninstalled.
//
// Success is the upgraded binary's version, never the manager's exit status, and it is read
// however the run ended: any release newer than r.From is installed, r.To or one between
// that a lagging tap still names, also when the relay stopped meanwhile. A manager that
// leaves r.From in place without failing (a tap or registry not updated yet) is
// ErrNotOffered, one that found another of its processes running is ErrBusy, and any other
// failure, Homebrew refusing an untrusted tap or npm a prefix it may not write (EACCES), is
// a failed install.
//
// Homebrew upgrades terma alone, without the cleanup that would prune the developer's other
// packages or upgrades of their dependents; a developer's HOMEBREW_NO_AUTO_UPDATE, and
// Homebrew's own brew.env files, still apply. npm is asked first whether its registry has
// the release, then installs exactly that version with its install script, which downloads
// the binary, run whatever ignore-scripts says: a lagging registry never has npm install the
// same release again, and the package never lands without its binary.
func (m Manager) Upgrade(ctx context.Context, r UpgradeRequest) (string, error) {
	// A relay told to stop starts nothing that would outlive it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(m.Argv) == 0 || m.Project != "" {
		return "", fmt.Errorf("terma cannot run %s for this installation; run `%s`", m.Name, m.Command)
	}
	env := m.backgroundEnv(os.Environ(), r.RefreshTaps)
	argv := m.Argv
	if m.Name == "npm" {
		// npm answers a version its registry does not have with E404, as it does a registry
		// it cannot reach: both are asked again soon.
		offered, err := probe(ctx, env, m.Argv[0], "view", npmPackage+"@"+r.To, "version")
		if err != nil {
			return "", fmt.Errorf("npm view found no terma %s (%w): %w", r.To, err, ErrNotOffered)
		}
		if offered != r.To {
			return "", fmt.Errorf("npm view found no terma %s: %w", r.To, ErrNotOffered)
		}
		argv = append(slices.Clone(m.Argv[:len(m.Argv)-1]), "--ignore-scripts=false", npmPackage+"@"+r.To)
	}
	log := openLog(r.Log)
	if log != nil {
		defer func() { _ = log.Close() }()
		fmt.Fprintf(log, "%s terma: upgrading %s → %s with %s: %s\n", time.Now().UTC().Format(time.RFC3339), r.From, r.To, m.Name, strings.Join(argv, " "))
	}
	ran := RunDetached(ctx, logWriter(log), env, argv...)
	// Whatever became of the run, the link says what is installed now: the manager may have
	// put a release in place before the relay stopped, or before its time ran out.
	got, err := InstalledVersion(context.WithoutCancel(ctx), m.Terma)
	switch {
	case err == nil && Newer(r.From, got):
		return got, nil
	case errors.Is(ctx.Err(), context.Canceled):
		return "", ctx.Err()
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s did not finish upgrading terma to %s in time (%w); its output is in %s", m.Name, r.To, ctx.Err(), r.Log)
	case ran != nil && busy(log):
		return "", fmt.Errorf("%s could not upgrade terma to %s (%w): %w", m.Name, r.To, ran, ErrBusy)
	case ran != nil:
		return "", fmt.Errorf("%s could not upgrade terma to %s (%w); its output is in %s", m.Name, r.To, ran, r.Log)
	case err != nil:
		return "", fmt.Errorf("%s upgraded terma, but %s does not run (%w); its output is in %s", m.Name, m.Terma, err, r.Log)
	}
	return "", fmt.Errorf("%s left terma at %s, not %s: %w", m.Name, got, r.To, ErrNotOffered)
}

// backgroundEnv is environ for the package manager, run with no one at a terminal.
func (m Manager) backgroundEnv(environ []string, refreshTaps bool) []string {
	switch m.Name {
	case "Homebrew":
		environ = append(environ,
			// Ask mode is Homebrew's default, though without a terminal it only prints its plan.
			"HOMEBREW_NO_ASK=1",
			// Leave the developer's other packages alone.
			"HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1",
			"HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_UPDATE_REPORT_NEW=1")
		if refreshTaps {
			// Update the taps first, whenever they were last updated.
			environ = append(environ, "HOMEBREW_AUTO_UPDATE_SECS=0")
		}
		return environ
	case "npm":
		// npm and the package's install script run on node, which a service's minimal PATH
		// lacks: it sits beside the npm that owns this terma, or in its prefix's bin.
		path := filepath.Dir(m.Argv[0]) + string(os.PathListSeparator) + filepath.Join(m.prefix, "bin")
		if old := envValue(environ, "PATH"); old != "" {
			path += string(os.PathListSeparator) + old
		}
		return append(environ, "PATH="+path,
			"npm_config_update_notifier=false", "npm_config_fund=false", "npm_config_audit=false")
	}
	return environ
}

// envValue is key's value in environ, the last one set; "" when it is not.
func envValue(environ []string, key string) string {
	value := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			value = v
		}
	}
	return value
}

// openLog is a new log at path for this upgrade, the last one's moved to <path>.1; nil when
// it cannot be made. It is this upgrade's alone, never the file of an earlier one its relay
// left to finish, which goes on writing to the file it was given: so what this one printed
// is all it holds (busy), and the log stays two upgrades long.
func openLog(path string) *os.File {
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return nil
	}
	_ = os.Rename(path, path+".1")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// logWriter is f as RunDetached's output, which discards it when there is no log.
func logWriter(f *os.File) io.Writer {
	if f == nil {
		return nil
	}
	return f
}

// busyMarks are what Homebrew prints when another of its processes holds a lock it needs.
var busyMarks = []string{"has already locked", "process is already running"}

// busyTail bounds how much of the end of an upgrade's output busy reads.
const busyTail = 64 << 10

// busy reports whether this upgrade's log, its own (openLog), says the manager found another
// of its processes holding its lock.
func busy(log *os.File) bool {
	if log == nil {
		return false
	}
	info, err := log.Stat()
	if err != nil {
		return false
	}
	from := max(info.Size()-busyTail, 0)
	tail := make([]byte, info.Size()-from)
	n, _ := log.ReadAt(tail, from)
	return slices.ContainsFunc(busyMarks, func(mark string) bool { return bytes.Contains(tail[:n], []byte(mark)) })
}

// RunDetached runs argv with environment env, its input empty and its output to out, so
// that the relay's service manager stopping the relay does not stop it too: in a session of
// its own, and, under systemd, in a scope of its own (scoped). At ctx's deadline it is
// killed with every process it started; when ctx is cancelled it is left to finish by
// itself, and out should be a file it can go on writing to.
func RunDetached(ctx context.Context, out io.Writer, env []string, argv ...string) error {
	// A done ctx starts nothing: what it would start would outlive a stopping relay.
	if err := ctx.Err(); err != nil {
		return err
	}
	argv = scoped(runtime.GOOS, func(k string) string { return envValue(env, k) }, exec.LookPath, scopeWorks(ctx, env), argv)
	// The probe takes a moment, in which the relay may have been told to stop.
	if err := ctx.Err(); err != nil {
		return err
	}
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
			_ = procinfo.KillGroup(c.Process)
			<-done
		}
		return ctx.Err()
	}
}

// scopeWorks is scoped's probe of systemd-run, run with environment env within probeTimeout,
// whatever becomes of ctx: a probe that a stopping relay cut short would read as no scope,
// and the program would run in the relay's unit, to be stopped with it.
func scopeWorks(ctx context.Context, env []string) func(argv ...string) bool {
	return func(argv ...string) bool {
		_, err := probe(context.WithoutCancel(ctx), env, argv...)
		return err == nil
	}
}

// scoped is argv run where the relay's service manager will not stop it with the relay. On
// Linux a relay systemd runs (INVOCATION_ID) is in its unit's control group, which systemd
// stops whole, other sessions included, so argv runs in a transient scope of its own
// (systemd-run --scope), when the user's systemd makes one (works); anywhere else, and
// should it not, a session of its own suffices.
func scoped(goos string, getenv func(string) string, lookPath func(string) (string, error), works func(argv ...string) bool, argv []string) []string {
	if goos != "linux" || getenv("INVOCATION_ID") == "" {
		return argv
	}
	run, err := lookPath("systemd-run")
	if err != nil {
		return argv
	}
	scope := []string{run, "--user", "--scope", "--quiet", "--collect", "--"}
	if !works(append(slices.Clone(scope), "true")...) {
		return argv
	}
	return append(scope, argv...)
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
