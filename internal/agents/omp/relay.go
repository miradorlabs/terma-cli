package omp

import (
	"context"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/pifamily"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay writes terma's exporter into omp, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	// Not the native exporter: it initializes before extensions, and its environment
	// would reach every tool omp runs.
	dir, err := ompAgentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	path, err := pifamily.Write(filepath.Join(dir, "extensions", "terma-relay.ts"), pifamily.ForRelay("omp", false, cfg))
	return agents.RelayResult{Paths: []string{path}}, err
}

// UnconfigureRelay removes terma's relay extension for omp.
func (Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	return relayexport.RemoveOwn(filepath.Join(dir, "extensions", "terma-relay.ts"))
}

var _ agents.RelayExporter = Agent{}
