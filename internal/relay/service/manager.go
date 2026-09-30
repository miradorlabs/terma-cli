package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// SuperviseLock is the lock `terma relay supervise` holds while it runs, under the
// relay's directory: how an install on Windows tells a supervisor is running.
const SuperviseLock = "supervise.lock"

// Manager installs, removes and finds the relay's per-user service for one config
// directory: launchd on macOS, systemd --user on Linux, and on Windows the per-user Run
// key starting `terma relay supervise`. The service runs `terma relay run --idle 0` and
// is restarted only when it exits nonzero.
type Manager struct {
	// Name is the service's label (Label).
	Name string
	// Exe is the terma the service runs; only Install needs it.
	Exe string
	// StateDir is the relay's directory: the Windows launcher and the launchd log live
	// there, and the supervisor's lock.
	StateDir string
	// Env is the environment the service runs with.
	Env map[string]string
	// StopRelay stops a relay a hook started, so the service can take its port and
	// lock; it returns once that relay has exited.
	StopRelay func()
	// StartSupervisor starts `terma relay supervise` now, detached (Windows).
	StartSupervisor func() error
}

// Label is the service's name: one per config directory, so a relay for a sandboxed
// config (tests, a second profile directory) never collides with the real one.
func Label(configDir, defaultDir string) string {
	name := "ai.terma.relay"
	if defaultDir == "" || filepath.Clean(configDir) != filepath.Clean(defaultDir) {
		sum := sha256.Sum256([]byte(filepath.Clean(configDir)))
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
		// The launcher the Run key starts at logon (manager_windows.go).
		return filepath.Join(m.StateDir, m.Name+".vbs"), nil
	}
	return "", errUnsupported()
}

// Installed reports whether the service definition exists, and where. It does not
// establish that the relay is running.
func (m Manager) Installed() (string, bool) {
	path, err := m.Path()
	if err != nil {
		return "", false
	}
	_, err = os.Stat(path)
	return path, err == nil
}

// Install writes the service definition and starts it, taking over from a relay a
// hook started, and returns where the definition is.
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
	switch runtime.GOOS {
	case "darwin":
		plist := Launchd(m.Name, m.Exe, filepath.Join(m.StateDir, "daemon.log"), m.Env)
		if err := config.WriteFileAtomic(path, []byte(plist), 0o644); err != nil {
			return "", err
		}
		var lastErr error
		for _, domain := range launchdDomains() {
			_, _ = run(ctx, "launchctl", "bootout", domain+"/"+m.Name)
			out, err := run(ctx, "launchctl", "bootstrap", domain, path)
			if err == nil {
				return path, nil
			}
			lastErr = fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, out)
		}
		return "", lastErr
	case "linux":
		if err := config.WriteFileAtomic(path, []byte(Systemd(m.Exe, m.Env)), 0o644); err != nil {
			return "", err
		}
		if out, err := run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return "", fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, out)
		}
		if out, err := run(ctx, "systemctl", "--user", "enable", "--now", m.Name+".service"); err != nil {
			return "", fmt.Errorf("systemctl --user enable: %w: %s", err, out)
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

// Remove stops the service and removes its definition, reporting whether there was
// one to remove.
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

// launchdDomains are the launchd domains to try: a login session's, then the user's
// background one (a machine with nobody logged in at the console, such as CI).
func launchdDomains() []string {
	uid := strconv.Itoa(os.Getuid())
	return []string{"gui/" + uid, "user/" + uid}
}
