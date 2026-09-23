//go:build unix

package hookrun

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func configureRendererProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// CommandContext normally kills only the shell, leaving grandchildren holding
	// stdout/stderr open. Cancel the renderer's process group instead.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A descendant that starts a separate session may retain output pipes even
	// after the group is killed. Bound pipe draining as well as process waiting.
	cmd.WaitDelay = rendererPipeDrain
}

func forwardRendererSignals(cmd *exec.Cmd, exited <-chan struct{}) {
	// Claude Code cancels an in-flight status line by terminating it. Pass that
	// on to the renderer's process group rather than leaving it drawing into a
	// pipe nobody reads.
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

func rendererSignalExitCode(exit *exec.ExitError) int {
	// Killed by a signal: report it the way a shell would.
	if st, ok := exit.Sys().(syscall.WaitStatus); ok && st.Signaled() {
		return 128 + int(st.Signal())
	}
	return -1
}
