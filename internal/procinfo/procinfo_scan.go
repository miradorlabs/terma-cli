//go:build darwin || linux

package procinfo

import "strings"

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

// splitEnv lists the NUL-terminated KEY=VALUE strings in b, up to the first empty one.
func splitEnv(b []byte) []string {
	var out []string
	for s := range strings.SplitSeq(string(b), "\x00") {
		if s == "" {
			break
		}
		out = append(out, s)
	}
	return out
}
