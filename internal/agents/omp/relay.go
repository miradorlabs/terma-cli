package omp

import (
	"context"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/pifamily"
)

// ConfigureRelay writes terma's exporter into omp, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	// omp's native exporter initializes before extensions, and its environment would
	// reach every tool it runs; terma's extension exports from its events.
	// A different file from the one `terma connect omp` writes (hooks/pre/terma.ts); one
	// in extensions/ loads the same way (verified on 18.3).
	dir, err := ompAgentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	path, err := pifamily.Write(filepath.Join(dir, "extensions", "terma-relay.ts"), pifamily.ForRelay("omp", false, cfg))
	return agents.RelayResult{Paths: []string{path}}, err
}

// RelayPointed is unknown: the exporter is terma's own, written by ConfigureRelay.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
