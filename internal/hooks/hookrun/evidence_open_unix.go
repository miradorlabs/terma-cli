//go:build unix

package hookrun

import "syscall"

// EvidenceOpenFlags follow no symlink and block on no special file swapped in after the Lstat.
const EvidenceOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
