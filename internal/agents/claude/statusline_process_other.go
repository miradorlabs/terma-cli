//go:build !unix

package claude

import "os/exec"

func configureRendererProcess(cmd *exec.Cmd) {
	// No process groups: the bounded wait closes the pipes but leaves descendants running.
	cmd.WaitDelay = rendererPipeDrain
}

func forwardRendererSignals(*exec.Cmd, <-chan struct{}) {}

func rendererSignalExitCode(*exec.ExitError) int { return -1 }
