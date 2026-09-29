package relay

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// systemdUnit names the Linux user unit.
const systemdUnit = "terma-relay.service"

func unitPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", systemdUnit), nil
}

func systemctl(ctx context.Context, args ...string) ([]byte, error) {
	return runCommand(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

func installService(ctx context.Context, binary string) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	want := systemdUnitFile(binary, serviceEnv())
	have, _ := os.ReadFile(path)
	state, _ := serviceState(ctx)
	if bytes.Equal(have, want) && state.Running {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, want, 0o644); err != nil {
		return err
	}
	if _, err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := systemctl(ctx, "enable", systemdUnit); err != nil {
		return err
	}
	_, err = systemctl(ctx, "restart", systemdUnit)
	return err
}

func uninstallService(ctx context.Context) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	_, _ = systemctl(ctx, "disable", "--now", systemdUnit)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, _ = systemctl(ctx, "daemon-reload")
	return nil
}

func restartService(ctx context.Context) error {
	state, err := serviceState(ctx)
	if err != nil || !state.Installed {
		return err
	}
	_, err = systemctl(ctx, "restart", systemdUnit)
	return err
}

func serviceState(ctx context.Context) (ServiceState, error) {
	path, err := unitPath()
	if err != nil {
		return ServiceState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, err
	}
	s := ServiceState{Installed: true}
	if m := unitExecRe.FindSubmatch(data); m != nil {
		s.Binary = systemdUnquote(string(m[1]))
	}
	out, _ := systemctl(ctx, "is-active", systemdUnit)
	active := strings.TrimSpace(string(out))
	s.Loaded = active != "" && active != "inactive" && active != "unknown"
	s.Running = active == "active"
	return s, nil
}

// RecordSupervisor is Windows' (service_windows.go); here the service manager
// supervises the relay.
func RecordSupervisor() error { return nil }

// HiddenProcess is how a supervised relay is started: with no window of its own on
// Windows, as is elsewhere.
func HiddenProcess() *syscall.SysProcAttr { return nil }
