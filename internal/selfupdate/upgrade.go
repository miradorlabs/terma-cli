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

// probeTimeout bounds asking the upgraded terma its version, and systemd-run whether it
// makes a scope.
const probeTimeout = time.Minute

// ErrNotOffered means Homebrew does not offer a published release yet: its tap is published
// after the release itself. It is no failed install, held back a day, but tried again soon
// (Attempt.Retries).
var ErrNotOffered = errors.New("its package manager does not offer it yet; terma asks again later")

// ErrBusy means another of Homebrew's processes held its lock, as the developer's own
// `brew upgrade` does while it runs: tried again soon too.
var ErrBusy = errors.New("another of its package manager's processes is running; terma asks again later")

// errLeftRunning means a program RunDetached started was left to finish when its ctx was
// cancelled: a package manager that may go on to install the release (Attempt.stopped).
var errLeftRunning = errors.New("left to finish by itself")

// logNote says where an upgrade's output is: the log at path, kept as <path>.1 once the next
// upgrade begins (openLog), so what doctor shows of a failure stays true.
func logNote(path string) string {
	return "its output is in " + path + " (" + filepath.Base(path) + ".1 once terma tries again)"
}

// UpgradeRequest is one upgrade of terma through Homebrew.
type UpgradeRequest struct {
	// From is the release installed now, and To the one to install.
	From, To string
	// Log is the file Homebrew's output goes to, a new one for each upgrade, the last one's
	// kept as <Log>.1 (openLog): a file, which Homebrew goes on writing to should the relay
	// stop first.
	Log string
	// RefreshTaps has Homebrew update its taps before the upgrade, which it otherwise does
	// once a day: the first few tries at a release ask for it, not every retry.
	RefreshTaps bool
}

// Upgrade has Homebrew install release r.To of terma in the background, as the relay does,
// and returns the version the terma at Terma then reports; the relay updates every other
// install in place, an npm package's binary included, and never runs npm. Nothing waits on
// a terminal: Homebrew's input is empty, it is never asked to prompt, and what it prints goes
// to r.Log. It runs for UpgradeTimeout at most, after which it is stopped with every process
// it started; ctx ending as the relay stops leaves it to finish by itself (RunDetached),
// since stopping it halfway can leave terma uninstalled.
//
// Success is the upgraded binary's version, never Homebrew's exit status, and it is read
// however the run ended: any release newer than r.From is installed, r.To or one between
// that a lagging tap still names, also when the relay stopped meanwhile. One that leaves
// r.From in place without failing (a tap not updated yet) is ErrNotOffered, one that found
// another of its processes running is ErrBusy, and any other failure, such as an untrusted
// tap, is a failed install. It upgrades terma alone, without the cleanup that would prune
// the developer's other packages or upgrades of their dependents; a developer's
// HOMEBREW_NO_AUTO_UPDATE, and Homebrew's own brew.env files, still apply.
func (m Manager) Upgrade(ctx context.Context, r UpgradeRequest) (string, error) {
	// A relay told to stop starts nothing that would outlive it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.Name != "Homebrew" || len(m.Argv) == 0 {
		return "", fmt.Errorf("terma cannot run %s for this installation; run `%s`", m.Name, m.Command)
	}
	env, argv := m.backgroundEnv(os.Environ(), r.RefreshTaps), m.Argv
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
	case errors.Is(ctx.Err(), context.Canceled) && errors.Is(ran, errLeftRunning):
		return "", ran
	case errors.Is(ctx.Err(), context.Canceled):
		return "", ctx.Err()
	case ctx.Err() != nil:
		return "", fmt.Errorf("%s did not finish upgrading terma to %s in time (%w); %s", m.Name, r.To, ctx.Err(), logNote(r.Log))
	case ran != nil && busy(log):
		return "", fmt.Errorf("%s could not upgrade terma to %s (%w): %w", m.Name, r.To, ran, ErrBusy)
	case ran != nil:
		return "", fmt.Errorf("%s could not upgrade terma to %s (%w); %s", m.Name, r.To, ran, logNote(r.Log))
	case err != nil:
		return "", fmt.Errorf("%s upgraded terma, but %s does not run (%w); %s", m.Name, m.Terma, err, logNote(r.Log))
	}
	return "", fmt.Errorf("%s left terma at %s, not %s: %w", m.Name, got, r.To, ErrNotOffered)
}

// backgroundEnv is environ for Homebrew, run with no one at a terminal.
func (m Manager) backgroundEnv(environ []string, refreshTaps bool) []string {
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
// itself (errLeftRunning), and out should be a file it can go on writing to.
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
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", ctx.Err(), errLeftRunning)
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
		// Its first line says why.
		if why, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); why != "" {
			return "", fmt.Errorf("%w: %s", err, why)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
