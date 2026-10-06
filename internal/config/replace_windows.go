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

// Rename is os.Rename, retried while another handle holds a file open: Windows refuses to
// move or replace a file someone is reading, where Unix goes ahead.
func Rename(from, to string) error { return whileShared(func() error { return os.Rename(from, to) }) }

// Remove is os.Remove, retried while another handle holds the file open.
func Remove(path string) error { return whileShared(func() error { return os.Remove(path) }) }

// whileShared retries op for about half a second while it fails because the file is open.
func whileShared(op func() error) error {
	for wait := time.Millisecond; ; wait *= 2 {
		err := op()
		var errno syscall.Errno
		if err == nil || !errors.As(err, &errno) || errno != syscall.ERROR_ACCESS_DENIED && errno != errSharingViolation || wait > 256*time.Millisecond {
			return err
		}
		time.Sleep(wait)
	}
}
