// Package procinfo tells the local relay which processes a hook runs under, which
// process owns the other end of a loopback connection, and whether a process still
// runs, so a claim covers only the agent process that made it; and doctor the
// environment an agent process started with.
package procinfo

import (
	"net/netip"
	"os"
)

// maxDepth bounds an ancestry walk; a real chain is a handful of processes deep.
const maxDepth = 32

// Ancestors returns the calling process's ancestors, nearest first, stopping before
// pid 1, which would match any sender.
func Ancestors() []int {
	var out []int
	pid := os.Getppid()
	for range maxDepth {
		if pid <= 1 {
			break
		}
		out = append(out, pid)
		next, ok := parentOf(pid)
		if !ok || next == pid {
			break
		}
		pid = next
	}
	return out
}

// Environ returns the environment pid started with, as os.Environ lists it; false when it
// cannot be read: the process is gone or another user's, or the platform does not say. An
// empty one is unread too: macOS withholds a platform binary's, and a zombie has none.
func Environ(pid int) ([]string, bool) {
	if pid <= 1 {
		return nil, false
	}
	env, ok := environ(pid)
	return env, ok && len(env) > 0
}

// FindSender returns the process holding the client end of a loopback connection from
// client to server, asking the kernel (about 3 ms over 900 processes on macOS), never a
// subprocess. The whole connection is matched, both addresses and both ports: another
// process can hold a socket with the same port numbers on other addresses (an IPv6 one, or
// a connection out of the machine), and matching the client's port alone named that
// process, the newest first.
func FindSender(client, server netip.AddrPort) (pid int, ok bool) {
	if !Supported {
		return 0, false
	}
	return findSender(conn{client: unmapped(client), server: unmapped(server)}, os.Getpid())
}

// conn is one TCP connection by its two ends, an IPv4-mapped address as IPv4.
type conn struct{ client, server netip.AddrPort }

func unmapped(a netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(a.Addr().Unmap(), a.Port()) }

// is reports whether a socket from local to remote is c's client end.
func (c conn) is(local, remote netip.AddrPort) bool {
	return unmapped(local) == c.client && unmapped(remote) == c.server
}
