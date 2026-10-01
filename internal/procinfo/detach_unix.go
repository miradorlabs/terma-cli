//go:build unix

package procinfo

import (
	"os/exec"
	"syscall"
)

// Detach puts a background process in its own session, so it outlives the hook's process group.
func Detach(proc *exec.Cmd) {
	proc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
