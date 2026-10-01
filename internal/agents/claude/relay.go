package claude

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay points Claude Code's own exporter at the local relay.
func (Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return relayexport.Native(exporter{}, cfg)
}

// RelayPointed reports whether Claude Code's exporter sends to the relay at addr.
func (Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(exporter{}, addr), true
}

var _ agents.RelayExporter = Agent{}

// Tool is the label Claude Code's hooks and trailers carry.
func (Agent) Tool() string { return "claude-code" }

var _ agents.Labeled = Agent{}
