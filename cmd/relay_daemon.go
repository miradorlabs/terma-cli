package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

// The relay as a per-user service: launchd on macOS, systemd --user on Linux, and on
// Windows the per-user Run key starting `terma relay supervise` (relay_supervise.go).
// `terma install` sets it up by default (relayServiceWanted). Without it hooks start a
// relay on demand, which cannot close two gaps: what an agent exports before its first
// hook (Codex's conversation_starts) when no relay runs, and anything exported while
// none does. The service runs `terma relay run --idle 0` and is restarted when it exits
// nonzero — a crash, or ExitRestart when the relay steps aside for a replaced binary, so
// an update takes effect without anyone restarting it. A relay that exits 0 found its
// setup gone (terma uninstalled) and stays stopped.

// relayServiceName is the service's label: one per config directory (service.Label).
func relayServiceName() (string, error) {
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

// relayServiceExecutable is the terma the service runs: the path this one was started
// as, not its resolved target — a package manager's upgrade removes the old target, and
// the service must start whatever the stable path points at next.
func relayServiceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

// relayService is this config directory's relay service. Exe is left to install, the
// one operation that runs it.
func relayService() (service.Manager, error) {
	name, err := relayServiceName()
	if err != nil {
		return service.Manager{}, err
	}
	dir, err := relayDir()
	if err != nil {
		return service.Manager{}, err
	}
	return service.Manager{Name: name, StateDir: dir, Env: serviceEnv(), StopRelay: func() { stopRelay(dir) }}, nil
}

func installRelayService(ctx context.Context) (string, error) {
	if !service.Supported() {
		return "", fmt.Errorf("a relay service is not supported on %s; hooks start the relay on demand", runtime.GOOS)
	}
	m, err := relayService()
	if err != nil {
		return "", err
	}
	if m.Exe, err = relayServiceExecutable(); err != nil {
		return "", err
	}
	if _, err := relayToken(); err != nil {
		return "", err
	}
	m.StartSupervisor = func() error {
		sup := exec.Command(m.Exe, "relay", "supervise")
		detach(sup)
		if err := sup.Start(); err != nil {
			return err
		}
		return sup.Process.Release()
	}
	return m.Install(ctx)
}

func removeRelayService(ctx context.Context) (bool, error) {
	m, err := relayService()
	if err != nil {
		return false, err
	}
	return m.Remove(ctx)
}

// relayServiceInstalled reports whether this config directory has a relay service
// definition, and where. It does not establish that the relay is running.
func relayServiceInstalled() (string, bool) {
	m, err := relayService()
	if err != nil {
		return "", false
	}
	return m.Installed()
}

func newRelayDaemonCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the relay as a per-user service (launchd, systemd --user, Windows's Run key)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install and start the relay service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			relayServiceWanted("on") // an explicit install clears an opt-out
			path, err := installRelayService(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The relay runs as a service (%s): always on, restarted by the system.\n", path)
			return nil
		},
	}, &cobra.Command{
		Use:   "remove",
		Short: "Stop and remove the relay service (hooks start the relay on demand again)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := removeRelayService(cmd.Context())
			if err != nil {
				return err
			}
			relayServiceWanted("off") // and install does not put it back
			if removed {
				fmt.Fprintln(cmd.OutOrStdout(), "The relay service is removed; hooks start the relay on demand.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "No relay service was installed.")
			}
			return nil
		},
	})
	return cmd
}
