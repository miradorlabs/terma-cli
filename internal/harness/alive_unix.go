//go:build unix

package harness

import (
	"errors"
	"syscall"
)

// EPERM means the process exists under another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
