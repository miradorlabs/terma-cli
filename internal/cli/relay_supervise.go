package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/relay/service"
)

func (app *App) newRelaySuperviseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "supervise",
		Short: "Keep the relay running while its service is installed (Windows)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := daemon.Dir(app.stateDir)
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
			svc, err := daemon.Service(app.stateDir)
			if err != nil {
				return err
			}
			definition, err := svc.Path()
			if err != nil {
				return err
			}
			// An install that rewrites the definition retires this supervisor.
			want, err := os.ReadFile(definition)
			if err != nil {
				return fmt.Errorf("the relay service is not installed: %w", err)
			}
			exe, err := procinfo.AbsExecutable()
			if err != nil {
				return err
			}
			daemon.Supervise(cmd.Context(), daemon.Supervisor{
				Start: func() *exec.Cmd {
					c := exec.Command(exe, "relay", "run", "--service", "--idle", "0", "--quiet")
					procinfo.Detach(c)
					return c
				},
				Installed: func() bool {
					have, err := os.ReadFile(definition)
					if err != nil || !bytes.Equal(have, want) {
						return false
					}
					_, err = daemon.Token(app.stateDir)
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
