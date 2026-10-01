// Package procinfo tells the local relay which processes a hook runs under, which
// process owns the other end of a loopback connection, and whether a process still
// runs, so a claim covers only the agent process that made it.
package procinfo

import "os"

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

// FindSender returns the process holding the client end of a loopback connection from
// port, asking the kernel (about 3 ms over 900 processes on macOS), never a subprocess.
func FindSender(port int) (pid int, ok bool) {
	if !Supported {
		return 0, false
	}
	return findSender(port, os.Getpid())
}
