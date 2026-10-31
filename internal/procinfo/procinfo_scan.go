//go:build darwin || linux

package procinfo

// children asks every process for its parent: neither platform lists a process's children.
func children(pid int) ([]int, bool) {
	all := allPIDs()
	if len(all) == 0 {
		return nil, false
	}
	var out []int
	for _, p := range all {
		if ppid, ok := parentOf(p); ok && ppid == pid {
			out = append(out, p)
		}
	}
	return out, true
}

// findSender asks every process whether it owns c's client end: neither platform has a
// table naming a socket's owner.
func findSender(c conn, self int) (int, bool) {
	for _, p := range allPIDs() {
		if p != self && ownsConn(p, c) {
			return p, true
		}
	}
	return 0, false
}
