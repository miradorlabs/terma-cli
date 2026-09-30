package claude

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ConfigureRelay points Claude's own exporter at the local relay.
func (Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return relayexport.Native(harness.Claude{}, cfg)
}

// RelayPointed reports whether Claude's exporter sends to the relay at addr.
func (Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(harness.Claude{}, addr), true
}

var _ agents.RelayExporter = Agent{}

// Tool is the label Claude Code's hooks and trailers carry.
func (Agent) Tool() string { return "claude-code" }

var _ agents.Labeled = Agent{}
