//go:build !windows

package cmd

import "os"

// stableExecutable is the path this terma was started as: os.Executable, which an
// update that replaces the file in place keeps naming.
func stableExecutable() string {
	exe, _ := os.Executable()
	return exe
}
