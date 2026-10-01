//go:build !unix && !windows

package procinfo

// processAlive cannot tell here; a recorded daemon is taken at its word.
func processAlive(int) bool { return true }
