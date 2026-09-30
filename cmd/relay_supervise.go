package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// Windows has no per-user service manager that restarts a process: the Run key starts
// a program once, at logon. `terma relay supervise` is what it starts, and it does what
// launchd's KeepAlive and systemd's Restart=on-failure do elsewhere: it runs the relay
// and starts it again when it exits asking to be (nonzero: a crash, or the step aside
// after its binary was replaced), for as long as the service stays installed. A relay
// that exits 0 is done for good — its token is gone, terma was uninstalled — and the
// supervisor ends with it.

const (
	relaySuperviseLock = "supervise.lock"
	relaySupervisorPID = "supervisor.pid"
)

// supervisor is what superviseRelay needs, so a test can drive it without a relay.
type supervisor struct {
	// start returns the relay's command, afresh each time: a binary an update replaced
	// is the one started next.
	start func() *exec.Cmd
	// installed reports whether the service is still wanted; once it is not, the relay
	// is stopped and the supervisor returns.
	installed func() bool
	// stop asks the running relay to stop, and waits for it (stopRelay).
	stop func()
	logf func(string, ...any)
	// minPause doubles to maxPause while the relay keeps dying, and resets once it has
	// run for healthy; poll is how often installed is asked.
	minPause, maxPause, healthy, poll time.Duration
}

// superviseRelay runs the relay until ctx ends, the service is removed, or the relay
// exits 0.
func superviseRelay(ctx context.Context, sv supervisor) {
	pause := sv.minPause
	for ctx.Err() == nil && sv.installed() {
		cmd := sv.start()
		began := time.Now()
		if err := cmd.Start(); err != nil {
			sv.logf("start the relay: %v", err)
		} else {
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			exited, err := sv.watch(ctx, cmd, done)
			if !exited || err == nil {
				return
			}
		}
		if time.Since(began) >= sv.healthy {
			pause = sv.minPause
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		pause = min(pause*2, sv.maxPause)
	}
}

// watch waits for the relay to exit, true (with how it exited) when it did on its own;
// when ctx ends or the service is removed it stops the relay — asked, so it delivers
// what it accepted, and killed only if it will not go — and reports false.
func (sv supervisor) watch(ctx context.Context, cmd *exec.Cmd, done <-chan error) (bool, error) {
	tick := time.NewTicker(sv.poll)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			sv.logf("relay exited (%v)", err)
			return true, err
		case <-ctx.Done():
		case <-tick.C:
			if sv.installed() {
				continue
			}
		}
		sv.stop()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return false, nil
	}
}

func newRelaySuperviseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "supervise",
		Short: "Keep the relay running while its service is installed (Windows)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := relayDir()
			if err != nil {
				return err
			}
			unlock, err := flock.TryLock(filepath.Join(dir, relaySuperviseLock))
			if flock.IsBusy(err) {
				return nil // another supervisor runs this relay
			}
			if err != nil {
				return err
			}
			defer unlock()
			name, err := relayServiceName()
			if err != nil {
				return err
			}
			definition, err := relayServicePath(name)
			if err != nil {
				return err
			}
			// The definition as it was when this supervisor started: an install that
			// rewrites it (a new binary path, another environment) retires this one.
			want, err := os.ReadFile(definition)
			if err != nil {
				return fmt.Errorf("the relay service is not installed: %w", err)
			}
			pidPath := filepath.Join(dir, relaySupervisorPID)
			_ = config.WriteFileAtomicNoSync(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
			defer func() { _ = os.Remove(pidPath) }()
			exe := stableExecutable()
			superviseRelay(cmd.Context(), supervisor{
				start: func() *exec.Cmd {
					c := exec.Command(exe, "relay", "run", "--idle", "0", "--quiet")
					detach(c)
					return c
				},
				installed: func() bool {
					have, err := os.ReadFile(definition)
					if err != nil || !bytes.Equal(have, want) {
						return false
					}
					_, err = relayToken()
					return err == nil
				},
				stop:     func() { stopRelay(dir) },
				logf:     func(string, ...any) {},
				minPause: time.Second, maxPause: time.Minute, healthy: time.Minute, poll: time.Second,
			})
			return nil
		},
	}
}
