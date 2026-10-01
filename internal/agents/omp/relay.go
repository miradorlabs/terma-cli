package omp

import (
	"context"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/pifamily"
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

// RelayPointed is unknown: the exporter is terma's own.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
