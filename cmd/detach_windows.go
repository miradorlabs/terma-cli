//go:build windows

package cmd

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the background process with no console of its own — a hook's console
// would otherwise flash a window, and close it under the relay — and in its own process
// group, so the Ctrl-C that ends the agent does not end the relay or flush too.
func detach(proc *exec.Cmd) {
	proc.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
