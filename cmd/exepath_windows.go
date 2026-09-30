//go:build windows

package cmd

import (
	"os"
	"path/filepath"
)

// stableExecutable is the path this terma was started as. An update on Windows renames
// the running terma.exe aside and puts the new one at its name, and what the system
// reports for a running image may follow the rename: the path it was started as is the
// one that names the new build, which is what a supervisor starts next and what a relay
// compares itself with to know it was replaced.
func stableExecutable() string {
	if filepath.IsAbs(os.Args[0]) {
		return os.Args[0]
	}
	exe, _ := os.Executable()
	return exe
}
