//go:build darwin || linux

package procinfo

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
