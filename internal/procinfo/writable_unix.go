//go:build unix

package procinfo

import (
	"errors"

	"golang.org/x/sys/unix"
)

// WriteDenied reports whether this process may not create files in dir: its permissions
// refuse it, or its file system is read-only. Any other answer, such as a directory that
// does not exist, is no refusal. It asks without writing anything.
func WriteDenied(dir string) bool {
	err := unix.Access(dir, unix.W_OK)
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS)
}
