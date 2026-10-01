package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

func newRelaySuperviseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "supervise",
		Short: "Keep the relay running while its service is installed (Windows)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir()
			if err != nil {
				return err
			}
			unlock, err := flock.TryLock(filepath.Join(dir, service.SuperviseLock))
			if flock.IsBusy(err) {
				return nil // another supervisor runs this relay
			}
			if err != nil {
				return err
			}
			defer unlock()
			svc, err := daemon.Service()
			if err != nil {
				return err
			}
			definition, err := svc.Path()
			if err != nil {
				return err
			}
			// The definition as it was when this supervisor started: an install that
			// rewrites it (a new binary path, another environment) retires this one.
			want, err := os.ReadFile(definition)
			if err != nil {
				return fmt.Errorf("the relay service is not installed: %w", err)
			}
			pidPath := filepath.Join(dir, daemon.SupervisorPIDFile)
			_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
			defer func() { _ = os.Remove(pidPath) }()
			exe := procinfo.StableExecutable()
			daemon.Supervise(cmd.Context(), daemon.Supervisor{
				Start: func() *exec.Cmd {
					c := exec.Command(exe, "relay", "run", "--idle", "0", "--quiet")
					procinfo.Detach(c)
					return c
				},
				Installed: func() bool {
					have, err := os.ReadFile(definition)
					if err != nil || !bytes.Equal(have, want) {
						return false
					}
					_, err = daemon.Token()
					return err == nil
				},
				Stop:     func() { daemon.Stop(dir) },
				Logf:     func(string, ...any) {},
				MinPause: time.Second, MaxPause: time.Minute, Healthy: time.Minute, Poll: time.Second,
			})
			return nil
		},
	}
}
