package delivery

import "syscall"

// connectionBroken are the errors of a connection the peer or the network broke off:
// Winsock's own, which syscall.ECONNRESET and its kin, invented for Windows, never match.
var connectionBroken = []syscall.Errno{syscall.WSAECONNRESET, syscall.WSAECONNABORTED, syscall.ECONNRESET, syscall.ECONNABORTED}
