//go:build !unix

package procinfo

import "os"

// KillGroup kills the process; where there are no process groups, only it.
func KillGroup(proc *os.Process) error { return proc.Kill() }
