package pi

import (
	"context"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/pifamily"
)

// ConfigureRelay writes terma's exporter into Pi, sending to the local relay.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	dir, err := agentDir()
	if err != nil {
		return agents.RelayResult{}, err
	}
	path, err := pifamily.Write(filepath.Join(dir, "extensions", "terma.ts"), pifamily.ForRelay("pi", true, cfg))
	return agents.RelayResult{Paths: []string{path}}, err
}

// agentDir is Pi's user configuration directory: PI_CODING_AGENT_DIR, else ~/.pi/agent.
func agentDir() (string, error) {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent"), err
}

var _ agents.RelayExporter = Agent{}
