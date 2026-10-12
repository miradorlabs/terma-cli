//go:build darwin || linux

package procinfo

// findSender asks every process whether it owns the connection from port to server:
// neither platform has a table naming a socket's owner.
func findSender(port, server, self int) (int, bool) {
	for _, p := range allPIDs() {
		if p != self && ownsConn(p, port, server) {
			return p, true
		}
	}
	return 0, false
}
