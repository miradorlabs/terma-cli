package procinfo

// Alive reports whether pid names a running process, true where that cannot be told.
func Alive(pid int) bool { return processAlive(pid) }
