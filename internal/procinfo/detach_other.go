//go:build !unix && !windows

package procinfo

import "os/exec"

// Detach does nothing where there is no session to leave.
func Detach(*exec.Cmd) {}
