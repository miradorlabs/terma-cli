//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// On Windows the relay's service is a value under the per-user Run key — no
// administrator, nothing to register — naming wscript and the launcher script
// (Windows), which starts `terma relay supervise` hidden at every logon. The
// registry is written through its API, never `reg.exe`.

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// supervisorWait bounds how long install and remove wait for a supervisor to hand
// over: it notices its launcher changed within a second, then stops the relay, which
// delivers what it accepted within its grace.
const supervisorWait = 20 * time.Second

func (m Manager) installWindows(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	launcher := []byte(Windows(m.Exe, m.Env))
	if have, err := os.ReadFile(path); err == nil && string(have) == string(launcher) && supervisorRunning(m.StateDir) {
		return nil // installed as it is, and running
	}
	if err := config.WriteFileAtomic(path, launcher, 0o600); err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open the Run key: %w", err)
	}
	defer func() { _ = key.Close() }()
	wscript := filepath.Join(os.Getenv("SystemRoot"), "System32", "wscript.exe")
	if os.Getenv("SystemRoot") == "" {
		wscript = "wscript.exe"
	}
	if err := key.SetStringValue(m.Name, `"`+wscript+`" //B //Nologo "`+path+`"`); err != nil {
		return fmt.Errorf("write the Run key: %w", err)
	}
	// A supervisor started from the launcher this replaced retires on its own; start
	// this one's now rather than at the next logon.
	if !waitSupervisorGone(m.StateDir) {
		return errors.New("the previous relay supervisor did not stop; the new one starts at the next logon")
	}
	if m.StartSupervisor == nil {
		return nil
	}
	return m.StartSupervisor()
}

func (m Manager) removeWindows(path string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err == nil {
		if err := key.DeleteValue(m.Name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			_ = key.Close()
			return fmt.Errorf("remove the Run key's value: %w", err)
		}
		_ = key.Close()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Without its launcher the supervisor stops the relay and exits.
	if !waitSupervisorGone(m.StateDir) && m.StopRelay != nil {
		m.StopRelay()
	}
	return nil
}

func supervisorRunning(dir string) bool {
	unlock, err := flock.TryLock(filepath.Join(dir, SuperviseLock))
	if err == nil {
		unlock()
		return false
	}
	return flock.IsBusy(err)
}

func waitSupervisorGone(dir string) bool {
	for deadline := time.Now().Add(supervisorWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !supervisorRunning(dir) {
			return true
		}
	}
	return false
}
