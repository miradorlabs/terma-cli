package relay

import "context"

// Placeholders until the launchd and systemd implementations land (docs/RELAY.md,
// "Service"). They keep the contract compiling for the code built against it.

func installService(context.Context, string) error { return errUnsupported }

func uninstallService(context.Context) error { return nil }

func restartService(context.Context) error { return nil }

func serviceState(context.Context) (ServiceState, error) { return ServiceState{}, nil }

func probe(context.Context, Config) (Health, error) { return Health{}, errUnsupported }
