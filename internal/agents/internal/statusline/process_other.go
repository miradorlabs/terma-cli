//go:build !unix

package statusline

import "os/exec"

func configureProcess(cmd *exec.Cmd) {
	// No process groups: the bounded wait closes the pipes but leaves descendants running.
	cmd.WaitDelay = pipeDrain
}

func forwardSignals(*exec.Cmd, <-chan struct{}) {}

func signalExitCode(*exec.ExitError) int { return -1 }
