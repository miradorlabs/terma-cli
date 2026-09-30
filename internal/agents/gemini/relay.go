package gemini

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// ConfigureRelay writes terma's exporter into Gemini CLI, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	// Gemini has no headers setting, so the token rides the endpoint path.
	settings, extension, err := connectRelay(cfg.Endpoint+"/"+cfg.Token, cfg.HookCommand)
	return agents.RelayResult{Paths: []string{settings, extension}}, err
}

// RelayPointed is unknown: the exporter is terma's own, written by ConfigureRelay.
func (Agent) RelayPointed(string) (bool, bool) { return false, false }

var _ agents.RelayExporter = Agent{}
