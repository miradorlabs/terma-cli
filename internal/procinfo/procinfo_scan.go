//go:build darwin || linux

package procinfo

// findSender asks every process whether it owns c's client end: neither platform has a
// table naming a socket's owner. A process the sender started while the connection was
// open can hold it too, inherited (an agent's `terma hook`, started beside its exporter's
// connection), and macOS lists the newest process first: the sender is the eldest of the
// first owner's ancestors that holds it.
func findSender(c conn, self int) (int, bool) {
	for _, p := range allPIDs() {
		if p != self && ownsConn(p, c) {
			return eldestOwner(p, c, self), true
		}
	}
	return 0, false
}

// eldestOwner is the furthest of owner's ancestors, owner itself if none, that holds c's
// client end.
func eldestOwner(owner int, c conn, self int) int {
	pid := owner
	for range maxDepth {
		parent, ok := parentOf(pid)
		if !ok || parent <= 1 || parent == pid {
			break
		}
		if parent != self && ownsConn(parent, c) {
			owner = parent
		}
		pid = parent
	}
	return owner
}
