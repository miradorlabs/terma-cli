package opencode

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ConfigureRelay points OpenCode's own exporter at the local relay.
func (Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return relayexport.Native(harness.OpenCode{}, cfg)
}

// RelayPointed reports whether OpenCode's exporter sends to the relay at addr.
func (Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(harness.OpenCode{}, addr), true
}

var _ agents.RelayExporter = Agent{}
