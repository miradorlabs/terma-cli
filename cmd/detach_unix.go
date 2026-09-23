//go:build unix

package cmd

import (
	"os/exec"
	"syscall"
)

// detach puts the background flush in its own session so it outlives the hook
// and is not killed when git tears down the hook's process group.
func detach(proc *exec.Cmd) {
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
