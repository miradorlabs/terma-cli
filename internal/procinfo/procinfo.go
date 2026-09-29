// Package procinfo answers the two questions the local relay needs about processes:
// which processes a hook runs under (its ancestors — the agent is one of them), and
// which process owns the other end of a loopback connection (the agent exporting).
// Together they tie a session's claim to the agent process that made it, so the same
// session resumed later by another process somewhere else is not covered by it.
package procinfo

import "os"

// maxDepth bounds an ancestry walk; a real chain is a handful of processes deep.
const maxDepth = 32

// Ancestors returns the calling process's ancestors, nearest first, stopping before
// pid 1 (launchd or init, the ancestor of everything, which would match any sender).
// It returns what it could walk; an error part-way ends the list, never the caller.
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

// FindSender returns the process holding the client end of a loopback connection
// that arrived from port, looking at every process this user may inspect (others
// refuse, and are skipped): about 3 ms over 900 processes on macOS. The relay runs it
// once per connection, when the connection's first export arrives — while the socket
// still exists, so a record held for a claim that comes later keeps its sender.
func FindSender(port int) (pid int, ok bool) {
	if !Supported {
		return 0, false
	}
	self := os.Getpid()
	for _, p := range allPIDs() {
		if p != self && ownsPort(p, port) {
			return p, true
		}
	}
	return 0, false
}
