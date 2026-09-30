//go:build unix

package harness

import "syscall"

// EvidenceOpenFlags follow no symlink and block on no special file, including
// replacement races.
const EvidenceOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
