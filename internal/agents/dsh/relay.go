package dsh

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay writes terma's exporter into DeepSeek Harness, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	path, err := writePlugin(pluginConfig{Endpoint: cfg.Endpoint, Headers: relayexport.Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand})
	return agents.RelayResult{Paths: []string{path}}, err
}

// RelayPointed is unknown: the exporter is terma's own, written by ConfigureRelay.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
