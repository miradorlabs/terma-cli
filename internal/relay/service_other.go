//go:build !darwin && !linux

package relay

import (
	"context"
	"errors"
)

// errUnsupported is returned by InstallService on a platform without a supported service
// manager; callers check Supported first.
var errUnsupported = errors.New("the relay needs launchd (macOS) or systemd --user (Linux)")

func installService(context.Context, string) error { return errUnsupported }

func uninstallService(context.Context) error { return nil }

func restartService(context.Context) error { return nil }

func serviceState(context.Context) (ServiceState, error) { return ServiceState{}, nil }
