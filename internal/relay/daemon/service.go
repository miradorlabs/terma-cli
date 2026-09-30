package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// The relay as a per-user service: launchd on macOS, systemd --user on Linux, and on
// Windows the per-user Run key starting `terma relay supervise` (Supervise).
// `terma install` sets it up by default. Without it hooks start a
// relay on demand, which cannot close two gaps: what an agent exports before its first
// hook (Codex's conversation_starts) when no relay runs, and anything exported while
// none does. The service runs `terma relay run --idle 0` and is restarted when it exits
// nonzero — a crash, or ExitRestart when the relay steps aside for a replaced binary, so
// an update takes effect without anyone restarting it. A relay that exits 0 found its
// setup gone (terma uninstalled) and stays stopped.

// serviceName is the service's label: one per config directory (service.Label).
func serviceName() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	def, _ := defaultConfigDir()
	return service.Label(dir, def), nil
}

func defaultConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "terma"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "terma"), nil
}

// serviceEnv is the environment the service runs with: what places its config and its
// environment, nothing else of the caller's.
func serviceEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"TERMA_CONFIG_DIR", "TERMA_ENV", "XDG_CONFIG_HOME", "HOME", "USERPROFILE", "TERMA_RELAY_HOLD"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// Service is this config directory's relay service. Exe is left to install, the
// one operation that runs it.
func Service() (service.Manager, error) {
	name, err := serviceName()
	if err != nil {
		return service.Manager{}, err
	}
	dir, err := Dir()
	if err != nil {
		return service.Manager{}, err
	}
	return service.Manager{Name: name, StateDir: dir, Env: serviceEnv(), StopRelay: func() { Stop(dir) }}, nil
}

// InstallService writes and starts the relay service for this config directory.
func InstallService(ctx context.Context) (string, error) {
	if !service.Supported() {
		return "", fmt.Errorf("a relay service is not supported on %s; hooks start the relay on demand", runtime.GOOS)
	}
	m, err := Service()
	if err != nil {
		return "", err
	}
	if m.Exe, err = procinfo.AbsExecutable(); err != nil {
		return "", err
	}
	if _, err := Token(); err != nil {
		return "", err
	}
	m.StartSupervisor = func() error {
		sup := exec.Command(m.Exe, "relay", "supervise")
		procinfo.Detach(sup)
		if err := sup.Start(); err != nil {
			return err
		}
		return sup.Process.Release()
	}
	return m.Install(ctx)
}

// RemoveService stops and removes the relay service.
func RemoveService(ctx context.Context) (bool, error) {
	m, err := Service()
	if err != nil {
		return false, err
	}
	return m.Remove(ctx)
}

// ServiceInstalled reports whether this config directory has a relay service
// definition, and where. It does not establish that the relay is running.
func ServiceInstalled() (string, bool) {
	m, err := Service()
	if err != nil {
		return "", false
	}
	return m.Installed()
}
