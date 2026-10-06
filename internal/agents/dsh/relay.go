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

// UnconfigureRelay removes terma's DeepSeek Harness plugin and its insert.
func (Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	paths, err := removePlugin()
	return agents.RelayResult{Paths: paths}, err
}

var _ agents.RelayExporter = Agent{}
