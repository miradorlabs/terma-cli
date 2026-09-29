package relay

import (
	"context"
)

// ServiceState is what the service manager says about the relay.
type ServiceState struct {
	// Installed is true when the launchd agent or systemd unit exists.
	Installed bool
	// Loaded is true when the service manager knows the service (launchd has it
	// bootstrapped, systemd has it activating or active or failed).
	Loaded bool
	// Running is true when the service manager reports the relay running.
	Running bool
	// Binary is the terma the service runs.
	Binary string
}

// InstallService writes the launchd agent or systemd unit that runs `<binary> relay
// serve` and (re)starts it. It is idempotent: an unchanged service is left running.
func InstallService(ctx context.Context, binary string) error {
	return installService(ctx, binary)
}

// UninstallService stops the relay and removes its service definition. The state
// directory stays: undelivered records in it are delivered by the next relay.
func UninstallService(ctx context.Context) error {
	return uninstallService(ctx)
}

// RestartService restarts a running relay so a replaced binary serves. It does nothing
// when no service is installed.
func RestartService(ctx context.Context) error {
	return restartService(ctx)
}

// Service reports the service manager's view of the relay.
func Service(ctx context.Context) (ServiceState, error) {
	return serviceState(ctx)
}

// Health is what a running relay reports about itself.
type Health struct {
	// Version is the terma build serving.
	Version string `json:"version"`
	// Backlog is the number of accepted request bodies not yet delivered.
	Backlog int `json:"backlog"`
	// HeldProjects are projects with records waiting for a key on this machine.
	HeldProjects []string `json:"held_projects,omitempty"`
	// LastError is the most recent delivery failure, "" when the last attempt succeeded.
	LastError string `json:"last_error,omitempty"`
	// Counters are the relay's totals since it started.
	Counters Counters `json:"counters"`
}

// Probe asks the relay at c for its health, an error when it does not answer or refuses
// c's token.
func Probe(ctx context.Context, c Config) (Health, error) {
	return probe(ctx, c)
}
