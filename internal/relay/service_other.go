//go:build !darwin && !linux && !windows

package relay

import (
	"context"
	"errors"
	"syscall"
)

// errUnsupported is returned by InstallService on a platform without a supported service
// manager; callers check Supported first.
var errUnsupported = errors.New("the relay needs launchd (macOS) or systemd --user (Linux)")

func installService(context.Context, string) error { return errUnsupported }

func uninstallService(context.Context) error { return nil }

func restartService(context.Context) error { return nil }

func serviceState(context.Context) (ServiceState, error) { return ServiceState{}, nil }

// RecordSupervisor is Windows' (service_windows.go); elsewhere a service manager
// supervises the relay.
func RecordSupervisor() error { return nil }

// HiddenProcess is how a supervised relay is started: with no window of its own on
// Windows, as is elsewhere.
func HiddenProcess() *syscall.SysProcAttr { return nil }
