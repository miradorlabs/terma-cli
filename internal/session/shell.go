package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ShellSnapshot is the working tree's changed files as a shell tool call found them just
// before it ran, each with a fingerprint of its state, so the call's own edits can be told
// from what was already there.
type ShellSnapshot struct {
	SessionID string            `json:"session_id"`
	At        time.Time         `json:"at"`
	Files     map[string]string `json:"files"`
}

const (
	shellDir = "shell"
	// shellSnapshotTTL bounds a snapshot whose call never reported back (killed, or its
	// PostToolUse hook never ran).
	shellSnapshotTTL = 24 * time.Hour
)

// SaveShell records snap under key until TakeShell reads it, and ages out snapshots their
// calls never collected.
func (s *Store) SaveShell(key string, snap ShellSnapshot) error {
	if !ValidID(key) || !ValidID(snap.SessionID) {
		return fmt.Errorf("invalid shell snapshot key %q", key)
	}
	dir := filepath.Join(s.dir, shellDir)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), manifestExt) && time.Since(info.ModTime()) > shellSnapshotTTL {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return writeJSON(filepath.Join(dir, key+manifestExt), snap)
}

// TakeShell returns and forgets the snapshot saved under key; ok is false when there is
// none, so a call whose start went unseen is never diffed against nothing.
func (s *Store) TakeShell(key string) (snap ShellSnapshot, ok bool) {
	if !ValidID(key) {
		return ShellSnapshot{}, false
	}
	path := filepath.Join(s.dir, shellDir, key+manifestExt)
	err := readJSON(path, &snap)
	if errors.Is(err, fs.ErrNotExist) {
		return ShellSnapshot{}, false
	}
	_ = os.Remove(path)
	if err != nil || !ValidID(snap.SessionID) {
		return ShellSnapshot{}, false
	}
	if snap.Files == nil {
		snap.Files = map[string]string{}
	}
	return snap, true
}
