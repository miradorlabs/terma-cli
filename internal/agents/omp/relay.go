package omp

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ConfigureRelay writes terma's exporter into omp, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	// omp's native exporter initializes before extensions, and its environment would
	// reach every tool it runs; terma's extension exports from its events.
	path, err := harness.WriteOmpRelayExtension(relayexport.PiConfig(cfg))
	return agents.RelayResult{Paths: []string{path}}, err
}

// RelayPointed is unknown: the exporter is terma's own, written by ConfigureRelay.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
