//go:build windows

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// On Windows the relay's service is a value under the per-user Run key — no
// administrator, nothing to register — naming wscript and the launcher script
// (windowsLauncher), which starts `terma relay supervise` hidden at every logon. The
// registry is written through its API, never `reg.exe`.

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// supervisorWait bounds how long install and remove wait for a supervisor to hand
// over: it notices its launcher changed within a second, then stops the relay, which
// delivers what it accepted within its grace.
const supervisorWait = 20 * time.Second

func installWindowsService(_ context.Context, name, path, exe, dir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	launcher := []byte(windowsLauncher(exe, serviceEnv()))
	if have, err := os.ReadFile(path); err == nil && string(have) == string(launcher) && supervisorRunning(dir) {
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
	if err := key.SetStringValue(name, `"`+wscript+`" //B //Nologo "`+path+`"`); err != nil {
		return fmt.Errorf("write the Run key: %w", err)
	}
	// A supervisor started from the launcher this replaced retires on its own; start
	// this one's now rather than at the next logon.
	if !waitSupervisorGone(dir) {
		return errors.New("the previous relay supervisor did not stop; the new one starts at the next logon")
	}
	sup := exec.Command(exe, "relay", "supervise")
	detach(sup)
	if err := sup.Start(); err != nil {
		return err
	}
	return sup.Process.Release()
}

func removeWindowsService(_ context.Context, name, path string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err == nil {
		if err := key.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
			_ = key.Close()
			return fmt.Errorf("remove the Run key's value: %w", err)
		}
		_ = key.Close()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Without its launcher the supervisor stops the relay and exits.
	dir, err := relayDir()
	if err != nil {
		return err
	}
	if !waitSupervisorGone(dir) {
		stopRelay(dir)
	}
	return nil
}

func supervisorRunning(dir string) bool {
	unlock, err := flock.TryLock(filepath.Join(dir, relaySuperviseLock))
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
