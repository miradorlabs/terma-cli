//go:build windows

package procinfo

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Detach starts the process with no console and in its own process group, so neither a
// closing console nor the agent's Ctrl-C ends it.
func Detach(proc *exec.Cmd) {
	proc.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
