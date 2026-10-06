package gemini

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// ConfigureRelay writes terma's exporter into Gemini CLI, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	settings, extension, err := connectRelay(cfg.Endpoint+"/"+cfg.Token, cfg.HookCommand)
	return agents.RelayResult{Paths: []string{settings, extension}}, err
}

// UnconfigureRelay removes terma's exporter from Gemini CLI.
func (Agent) UnconfigureRelay(_ context.Context, token string) (agents.RelayResult, error) {
	paths, err := disconnectRelay(token)
	return agents.RelayResult{Paths: paths}, err
}

var _ agents.RelayExporter = Agent{}
