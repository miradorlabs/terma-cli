//go:build unix

package hookrun

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// openDirNoBlock opens dir as a directory without blocking on a FIFO or a symlink to one
// (O_DIRECTORY returns ENOTDIR immediately for non-directories, including FIFOs).
func openDirNoBlock(dir string) (*os.File, error) {
	for {
		fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		} else if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), dir), nil
	}
}

func openFileNoBlock(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
func isBenignDirErr(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}
