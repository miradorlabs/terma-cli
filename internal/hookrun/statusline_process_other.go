//go:build !unix

package hookrun

import "os/exec"

func configureRendererProcess(cmd *exec.Cmd) {
	// Keep CommandContext's portable Process.Kill cancellation. There are no Unix
	// process groups here; bound the wait if a descendant keeps output pipes open.
	// This closes the pipes but does not terminate descendants.
	cmd.WaitDelay = rendererPipeDrain
}

func forwardRendererSignals(*exec.Cmd, <-chan struct{}) {}

func rendererSignalExitCode(*exec.ExitError) int { return -1 }
