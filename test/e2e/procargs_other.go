//go:build !darwin

package e2e

import "errors"

// procArgs reads KERN_PROCARGS2, which only macOS has; the desktop profiles run on macOS alone.
func procArgs(int) (argv, env []string, err error) {
	return nil, nil, errors.New("procargs2: macOS only")
}
