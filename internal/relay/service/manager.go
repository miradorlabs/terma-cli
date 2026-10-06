package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// SuperviseLock is the lock `terma relay supervise` holds while it runs, under the relay's directory.
const SuperviseLock = "supervisor.lock"

// DaemonLogFile is the file under the relay's directory that takes the service's relay's stdout and stderr.
const DaemonLogFile = "daemon.log"

// Manager installs, removes and finds the relay's per-user service for one state directory;
// the service is restarted only when it exits nonzero.
type Manager struct {
	Name string
	// Exe is the terma the service runs; only Install needs it.
	Exe string
	// RelayDir is the relay's directory, which keeps the service's log and Windows launcher.
	RelayDir string
	Env      map[string]string
	// StopRelay stops a hook-started relay so the service can take its port and lock.
	StopRelay func()
	// StartSupervisor starts `terma relay supervise` now, detached (Windows).
	StartSupervisor func() error
}

// Label is the service's name, one per state directory so a sandbox never collides with the real one.
func Label(stateDir, defaultDir string) string {
	name := "ai.terma.relay"
	if defaultDir == "" || filepath.Clean(stateDir) != filepath.Clean(defaultDir) {
		sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
		name += "." + hex.EncodeToString(sum[:])[:10]
	}
	return name
}

// Supported reports whether this platform can run the relay as a per-user service.
func Supported() bool {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return true
	}
	return false
}

func errUnsupported() error {
	return fmt.Errorf("a relay service is not supported on %s; hooks start the relay on demand", runtime.GOOS)
}

// Path is where the service definition lives.
func (m Manager) Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", m.Name+".plist"), nil
	case "linux":
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "systemd", "user", m.Name+".service"), nil
	case "windows":
		return filepath.Join(m.RelayDir, m.Name+".vbs"), nil
	}
	return "", errUnsupported()
}

// Installed reports whether the service definition exists, and where, not whether the relay runs.
func (m Manager) Installed() (string, bool) {
	path, err := m.Path()
	if err != nil {
		return "", false
	}
	_, err = os.Stat(path)
	return path, err == nil
}

// Definition is the service definition Install writes for m, byte for byte.
func (m Manager) Definition() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return Launchd(m.Name, m.Exe, filepath.Join(m.RelayDir, DaemonLogFile), m.Env), nil
	case "linux":
		return Systemd(m.Exe, m.Env), nil
	case "windows":
		return Windows(m.Exe, m.Env), nil
	}
	return "", errUnsupported()
}

// Current reports whether the installed definition is the one Install would write now. One
// an earlier terma wrote, or for another binary or environment, is not: the system may run
// a command this terma no longer has, or a relay that talks to another backend. One that
// reaches this same binary by another path (a symlink, ./bin/terma) is: rewriting it would
// restart the relay for nothing, and an agent never resends what a restarting relay refused.
func (m Manager) Current() bool {
	path, err := m.Path()
	if err != nil {
		return false
	}
	have, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	want, err := m.Definition()
	if err != nil {
		return false
	}
	if string(have) == want {
		return true
	}
	exe, ok := m.installedExe(string(have))
	return ok && sameFile(exe, m.Exe)
}

// installedExe is the binary an installed definition runs, when it is the definition Install
// would write for that binary: everything but the path must match.
func (m Manager) installedExe(have string) (string, bool) {
	const mark = "TERMAEXEPLACEHOLDER"
	o := m
	o.Exe = mark
	tmpl, err := o.Definition()
	if err != nil || !strings.Contains(tmpl, mark) {
		return "", false
	}
	pieces := strings.Split(tmpl, mark)
	for i, p := range pieces {
		pieces[i] = regexp.QuoteMeta(p)
	}
	match := regexp.MustCompile("^" + strings.Join(pieces, "(.+?)") + "$").FindStringSubmatch(have)
	if match == nil {
		return "", false
	}
	// The renderers escape the path (XML, a Go-quoted string, VBScript quotes); the one
	// candidate that renders the installed bytes again is the path. Re-rendering also keeps
	// this right should a renderer ever write the path twice: every occurrence must match.
	raw := match[1]
	unquoted, _ := strconv.Unquote(`"` + raw + `"`)
	for _, exe := range []string{raw, html.UnescapeString(raw), unquoted, strings.ReplaceAll(raw, `""`, `"`)} {
		if exe == "" {
			continue
		}
		o.Exe = exe
		if def, err := o.Definition(); err == nil && def == have {
			return exe, true
		}
	}
	return "", false
}

func sameFile(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	return err == nil && os.SameFile(ia, ib)
}

// Install writes the service definition and starts it, and returns where the definition is.
func (m Manager) Install(ctx context.Context) (string, error) {
	if !Supported() {
		return "", errUnsupported()
	}
	path, err := m.Path()
	if err != nil {
		return "", err
	}
	// A relay a hook started holds the port and the lock: the service takes over.
	if m.StopRelay != nil {
		m.StopRelay()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	def, err := m.Definition()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		if err := config.WriteFileAtomic(path, []byte(def), 0o644); err != nil {
			return "", err
		}
		var lastErr error
		for _, domain := range launchdDomains() {
			// Only a job bootout unloaded can still be exiting.
			wait := time.Duration(0)
			if _, err := run(ctx, "launchctl", "bootout", domain+"/"+m.Name); err == nil {
				wait = bootoutWait
			}
			out, err := bootstrap(ctx, domain, path, wait)
			if err == nil {
				return path, nil
			}
			lastErr = fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, out)
		}
		return "", lastErr
	case "linux":
		if err := config.WriteFileAtomic(path, []byte(def), 0o644); err != nil {
			return "", err
		}
		if out, err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return "", fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, out)
		}
		if out, err := run(ctx, "systemctl", "--user", "enable", m.Name+".service"); err != nil {
			return "", fmt.Errorf("systemctl --user enable: %w: %s", err, out)
		}
		// Not start: a relay StopRelay just stopped would wait out RestartSec.
		if out, err := run(ctx, "systemctl", "--user", "restart", m.Name+".service"); err != nil {
			return "", fmt.Errorf("systemctl --user restart: %w: %s", err, out)
		}
		return path, nil
	case "windows":
		if err := m.installWindows(path); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("unsupported")
}

// Start starts the installed service's relay as it is defined, without rewriting it: one
// already running is left alone, and one that exited 0 or was unloaded runs again.
func (m Manager) Start(ctx context.Context) error { return m.start(ctx, false) }

// Restart is Start for a relay Stop has just stopped. On Linux its unit is restarted even
// while it still reads as active: the relay may not have exited yet, and once it has, a
// unit left to itself waits out RestartSec.
func (m Manager) Restart(ctx context.Context) error { return m.start(ctx, true) }

func (m Manager) start(ctx context.Context, stopped bool) error {
	path, ok := m.Installed()
	if !ok {
		return errors.New("the relay service is not installed")
	}
	switch runtime.GOOS {
	case "darwin":
		var lastErr error
		for _, domain := range launchdDomains() {
			if _, err := run(ctx, "launchctl", "kickstart", domain+"/"+m.Name); err == nil {
				return nil
			}
		}
		// Not loaded: kickstart knows only loaded jobs.
		for _, domain := range launchdDomains() {
			out, err := run(ctx, "launchctl", "bootstrap", domain, path)
			if err == nil {
				return nil
			}
			lastErr = fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, out)
		}
		return lastErr
	case "linux":
		if _, err := run(ctx, "systemctl", "--user", "is-active", "--quiet", m.Name+".service"); err == nil && !stopped {
			return nil
		}
		// Not start: a unit whose relay exited waits out RestartSec.
		if out, err := run(ctx, "systemctl", "--user", "restart", m.Name+".service"); err != nil {
			return fmt.Errorf("systemctl --user restart: %w: %s", err, out)
		}
		return nil
	case "windows":
		return m.startWindows()
	}
	return errUnsupported()
}

// Remove stops the service and removes its definition, reporting whether there was one.
func (m Manager) Remove(ctx context.Context) (bool, error) {
	path, err := m.Path()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	switch runtime.GOOS {
	case "darwin":
		for _, domain := range launchdDomains() {
			_, _ = run(ctx, "launchctl", "bootout", domain+"/"+m.Name)
		}
	case "linux":
		_, _ = run(ctx, "systemctl", "--user", "disable", "--now", m.Name+".service")
	case "windows":
		return true, m.removeWindows(path)
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	if runtime.GOOS == "linux" {
		_, _ = run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return true, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// bootoutWait outlasts launchd's 20 s exit timeout, after which it kills the relay bootout stopped.
const bootoutWait = 25 * time.Second

// bootstrap loads the agent in domain, retrying for up to wait while it fails with an I/O
// error (5): bootout returns before the relay it stopped has exited, and until then the job
// cannot be loaded again.
func bootstrap(ctx context.Context, domain, path string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		out, err := run(ctx, "launchctl", "bootstrap", domain, path)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 5 || time.Now().After(deadline) || ctx.Err() != nil {
			return out, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// launchdDomains end with the user's background domain for a machine nobody is logged in to, such as CI.
func launchdDomains() []string {
	uid := strconv.Itoa(os.Getuid())
	return []string{"gui/" + uid, "user/" + uid}
}
