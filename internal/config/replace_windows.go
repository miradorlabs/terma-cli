//go:build windows

package config

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// errSharingViolation is Windows' ERROR_SHARING_VIOLATION.
const errSharingViolation = syscall.Errno(32)

// replaceFile is os.Rename, retried while another handle holds the target open: Windows
// refuses to replace a file someone is reading, where Unix replaces it.
func replaceFile(from, to string) error {
	for wait := time.Millisecond; ; wait *= 2 {
		err := os.Rename(from, to)
		var errno syscall.Errno
		if err == nil || !errors.As(err, &errno) || errno != syscall.ERROR_ACCESS_DENIED && errno != errSharingViolation || wait > 256*time.Millisecond {
			return err
		}
		time.Sleep(wait)
	}
}
