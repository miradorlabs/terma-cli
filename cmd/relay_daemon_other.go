//go:build !windows

package cmd

import (
	"context"
	"errors"
)

// The Windows service (relay_daemon_windows.go) exists only there.

func installWindowsService(context.Context, string, string, string, string) error {
	return errors.New("not Windows")
}

func removeWindowsService(context.Context, string, string) error {
	return errors.New("not Windows")
}
