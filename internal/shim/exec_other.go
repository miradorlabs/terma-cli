//go:build !unix

package shim

import (
	"errors"
	"os"
	"os/exec"
)

// execReal runs the agent as a child on platforms without execve, then exits with its
// status so callers see the agent's exit code, not terma's.
func execReal(path string, args, env []string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}
