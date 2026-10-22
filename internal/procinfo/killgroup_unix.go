//go:build unix

package procinfo

import (
	"os"
	"syscall"
)

// KillGroup kills a process Detach started and every process it started in turn, which
// share its process group: a package manager's downloads and git, not only itself.
func KillGroup(proc *os.Process) error {
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
		return proc.Kill()
	}
	return nil
}
