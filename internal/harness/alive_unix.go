//go:build unix

package harness

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process. Signal 0 checks it exists;
// EPERM means it does, under another user.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
