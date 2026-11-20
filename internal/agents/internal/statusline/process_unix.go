//go:build unix

package statusline

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext kills only the shell; grandchildren would hold stdout open.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A descendant in its own session can outlive the group kill, so pipe draining is bounded too.
	cmd.WaitDelay = pipeDrain
}

func forwardSignals(cmd *exec.Cmd, exited <-chan struct{}) {
	// An agent cancels an in-flight status line by terminating it; pass that to the group.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		select {
		case sig := <-sigs:
			if s, ok := sig.(syscall.Signal); ok && cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		case <-exited:
		}
		signal.Stop(sigs)
	}()
}

func signalExitCode(exit *exec.ExitError) int {
	if st, ok := exit.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return -1
}
