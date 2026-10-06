package opencode

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay points OpenCode's own exporter at the local relay.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return relayexport.Native(exporter{dir: a.ConfigDir}, cfg)
}

// UnconfigureRelay restores OpenCode's own exporter settings to what they were before setup.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	return relayexport.Unnative(exporter{dir: a.ConfigDir})
}

// RelayPointed reports whether OpenCode's exporter sends to the relay at addr.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(exporter{dir: a.ConfigDir}, addr), true
}

var _ agents.RelayExporter = Agent{}
