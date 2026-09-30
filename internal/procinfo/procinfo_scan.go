//go:build darwin || linux

package procinfo

// findSender asks every process whether it owns the connection from port: neither
// platform has a table naming a socket's owner.
func findSender(port, self int) (int, bool) {
	for _, p := range allPIDs() {
		if p != self && ownsPort(p, port) {
			return p, true
		}
	}
	return 0, false
}
