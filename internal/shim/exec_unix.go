//go:build unix

package shim

import "syscall"

// execReal replaces the current process with the agent binary. execve carries stdio,
// the terminal, signal handling and the exit status through unchanged.
func execReal(path string, args, env []string) error {
	argv := append([]string{path}, args...)
	return syscall.Exec(path, argv, env)
}
