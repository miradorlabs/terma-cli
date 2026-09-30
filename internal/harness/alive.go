package harness

// ProcessAlive reports whether pid names a running process (true where that cannot be
// told). The relay attributes what names no session to its sender only once the sender
// has exited.
func ProcessAlive(pid int) bool { return processAlive(pid) }
