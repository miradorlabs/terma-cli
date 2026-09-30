//go:build !windows

package procinfo

import "os"

// StableExecutable is the path this terma was started as: os.Executable, which an
// update that replaces the file in place keeps naming.
func StableExecutable() string {
	exe, _ := os.Executable()
	return exe
}
