//go:build windows

package procinfo

import (
	"os"
	"path/filepath"
)

// StableExecutable is the path terma was started as, which names the new build after an
// update renames the running one aside.
func StableExecutable() string {
	if filepath.IsAbs(os.Args[0]) {
		return os.Args[0]
	}
	exe, _ := os.Executable()
	return exe
}
