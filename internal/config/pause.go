package config

import (
	"os"
	"path/filepath"
)

// pausedFile, while it exists, stops hooks and the relay capturing anything new.
const pausedFile = "paused"

// PausedPath is the machine-wide pause switch `terma pause` writes.
func PausedPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pausedFile), nil
}

// Paused reports whether the developer paused capture on this machine; callers also check
// Policy.PauseAllowed, since an organization can forbid it.
func Paused() bool {
	path, err := PausedPath()
	if err != nil {
		return false
	}
	_, err = os.Lstat(path)
	return err == nil
}

// PauseAllowed reports whether p lets a member pause: always in repository mode, and in
// global mode only when the organization allows it.
func (p Policy) PauseAllowed() bool { return !p.Global() || p.MembersCanPause }
