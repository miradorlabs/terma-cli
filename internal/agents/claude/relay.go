package claude

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay points Claude Code's own exporter at the local relay.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return relayexport.Native(exporter{dir: a.ConfigDir}, cfg)
}

// UnconfigureRelay restores Claude Code's own exporter settings to what they were before setup.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	return relayexport.Unnative(exporter{dir: a.ConfigDir})
}

// RelayPointed reports whether Claude Code's exporter sends to the relay at addr.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(exporter{dir: a.ConfigDir}, addr), true
}

var _ agents.RelayExporter = Agent{}

// Tool is the label Claude Code's hooks and trailers carry.
func (Agent) Tool() string { return "claude-code" }

var _ agents.Labeled = Agent{}
