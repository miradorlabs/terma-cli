//go:build windows

package relay

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// On Windows the relay runs at logon from the per-user Run key — no administrator, no
// service to register — through a hidden wscript launcher, so no console window opens,
// and `terma relay supervise` restarts it as launchd's KeepAlive does on macOS.

const (
	runKey      = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	runValue    = "TermaRelay"
	launcherVBS = "relay.vbs"
	pidFile     = "supervisor.pid"
)

func launcherPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, launcherVBS), nil
}

func installService(ctx context.Context, binary string) error {
	path, err := launcherPath()
	if err != nil {
		return err
	}
	want := windowsLauncher(binary, serviceEnv())
	have, _ := os.ReadFile(path)
	state, _ := serviceState(ctx)
	if bytes.Equal(have, want) && state.Running {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := config.WriteFileAtomic(path, want, 0o600); err != nil {
		return err
	}
	if _, err := runCommand(ctx, "reg", "add", runKey, "/v", runValue, "/t", "REG_SZ",
		"/d", `wscript.exe "`+path+`"`, "/f"); err != nil {
		return err
	}
	stopSupervisor(ctx)
	_, err = runCommand(ctx, "wscript.exe", path)
	return err
}

func uninstallService(ctx context.Context) error {
	_, _ = runCommand(ctx, "reg", "delete", runKey, "/v", runValue, "/f")
	stopSupervisor(ctx)
	path, err := launcherPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func restartService(ctx context.Context) error {
	state, err := serviceState(ctx)
	if err != nil || !state.Installed {
		return err
	}
	path, err := launcherPath()
	if err != nil {
		return err
	}
	stopSupervisor(ctx)
	_, err = runCommand(ctx, "wscript.exe", path)
	return err
}

func serviceState(ctx context.Context) (ServiceState, error) {
	path, err := launcherPath()
	if err != nil {
		return ServiceState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ServiceState{}, nil
	}
	if err != nil {
		return ServiceState{}, err
	}
	s := ServiceState{Binary: launcherBinary(data)}
	if _, err := runCommand(ctx, "reg", "query", runKey, "/v", runValue); err == nil {
		s.Installed = true
	}
	if pid := supervisorPID(); pid > 0 {
		out, err := runCommand(ctx, "tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH")
		s.Loaded = err == nil && strings.Contains(string(out), strconv.Itoa(pid))
		s.Running = s.Loaded
	}
	return s, nil
}

// supervisorPID is the running supervisor's process id, 0 when none is recorded.
func supervisorPID() int {
	dir, err := Dir()
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// stopSupervisor ends the supervisor and the relay it runs.
func stopSupervisor(ctx context.Context) {
	if pid := supervisorPID(); pid > 0 {
		_, _ = runCommand(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	}
}

// RecordSupervisor notes the running supervisor's process id, for status and restart.
func RecordSupervisor() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return config.WriteFileAtomicNoSync(filepath.Join(dir, pidFile), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// HiddenProcess starts a child with no console window of its own.
func HiddenProcess() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
