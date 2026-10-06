package hermes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// ConfigureRelay writes terma's exporter into Hermes, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	path, err := writePlugin(pluginConfig{Endpoint: cfg.Endpoint, Headers: relayexport.Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand})
	result := agents.RelayResult{Paths: []string{path}}
	if err != nil {
		return result, err
	}
	if err := enablePlugin(ctx); err != nil {
		result.Pending = true
		result.Notes = append(result.Notes, fmt.Sprintf("Hermes: the plugin is written (%s) but not enabled: %v.", path, err))
	}
	return result, nil
}

// UnconfigureRelay disables terma's Hermes plugin and removes it.
func (Agent) UnconfigureRelay(ctx context.Context, _ string) (agents.RelayResult, error) {
	dir, err := pluginDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return agents.RelayResult{}, nil
	}
	// Removing the files stops it either way; disabling keeps Hermes from looking for it.
	disabled := disablePlugin(ctx)
	result, err := relayexport.RemoveOwn(dir)
	if err == nil && disabled != nil {
		result.Notes = append(result.Notes, fmt.Sprintf("Hermes: removed the plugin (%s) but could not disable it: %v.", dir, disabled))
	}
	return result, err
}

var _ agents.RelayExporter = Agent{}
