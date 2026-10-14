//go:build !windows

package cli

import "syscall"

// execCommand replaces terma with the command, so the developer's shell, its signals and
// the command's exit status are the command's own.
func execCommand(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}
