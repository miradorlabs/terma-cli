//go:build unix

package harness

import "syscall"

// No symlink following or blocking special files, including replacement races.
const evidenceOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
