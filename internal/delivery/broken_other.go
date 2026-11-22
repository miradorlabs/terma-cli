//go:build !windows

package delivery

import "syscall"

// connectionBroken are the errors of a connection the peer or the network broke off.
var connectionBroken = []syscall.Errno{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE}
