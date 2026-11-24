//go:build unix

package hookrun

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// openDirNoBlock opens dir via O_DIRECTORY so FIFOs fail immediately with ENOTDIR.
func openDirNoBlock(dir string) (*os.File, error) {
	for {
		if fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); !errors.Is(err, syscall.EINTR) {
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), dir), nil
		}
	}
}
func openFileNoBlock(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
func isBenignDirErr(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}
