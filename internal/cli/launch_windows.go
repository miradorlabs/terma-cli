//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
)

// execCommand runs the command on terma's own streams and ends with its exit status:
// Windows cannot replace a process.
func execCommand(path string, argv, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitWith(exit.ExitCode())
	}
	return err
}
