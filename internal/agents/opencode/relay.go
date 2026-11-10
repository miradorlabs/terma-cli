package opencode

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay points OpenCode's own exporter at the local relay. The plugin runs `terma
// hook` by the path setup names: OpenCode 2 runs it in a background server, whose PATH is
// whatever the client that started it had.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	if version := installedVersion(); !supported(version) {
		return agents.RelayResult{Pending: true, Notes: []string{tooOld(version) + " Terma left OpenCode's plugins as they were; update OpenCode, then run `terma setup` again."}}, nil
	}
	return relayexport.Native(exporter{dir: a.ConfigDir, hook: cfg.HookCommand}, cfg)
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
