package hermes

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay writes terma's exporter into Hermes, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	path, err := writePlugin(pluginConfig{Endpoint: cfg.Endpoint, Headers: relayexport.Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand})
	result := agents.RelayResult{Paths: []string{path}}
	if err != nil {
		return result, err
	}
	if err := enablePlugin(ctx); err != nil {
		result.Pending = true
		result.Notes = append(result.Notes, fmt.Sprintf("Hermes: the plugin is written (%s) but not enabled: %v.", path, err))
	}
	return result, nil
}

// RelayPointed is unknown: the exporter is terma's own, written by ConfigureRelay.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
