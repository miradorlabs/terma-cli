package harness

// ProcessAlive reports whether pid names a running process, true where that cannot be told.
func ProcessAlive(pid int) bool { return processAlive(pid) }
