package dsh

import (
	"context"
	"encoding/json"
	"path/filepath"

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

// RefreshMachine rewrites terma's DeepSeek Harness plugin in this build's template; its
// insert in the home patch names the same path and stays.
func (Agent) RefreshMachine() (string, bool, error) {
	dir, err := home()
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(dir, "plugins", "terma-relay.mjs")
	changed, err := relayexport.Refresh(path, "const CONFIG = ", " /* terma:config */", func(raw string) (string, bool) {
		var cfg pluginConfig
		if json.Unmarshal([]byte(raw), &cfg) != nil || cfg.Endpoint == "" {
			return "", false
		}
		text, err := renderPlugin(cfg)
		return text, err == nil
	})
	return path, changed, err
}

var _ agents.MachineRefresher = Agent{}
