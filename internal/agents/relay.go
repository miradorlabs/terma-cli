package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// RelayConfig is where an agent's telemetry reaches the local relay. Token is never
// printed or put in an agent's tool environment.
type RelayConfig struct {
	Endpoint    string
	Token       string
	HookCommand []string
	StateDir    string
}

// RelayResult is what configuring an agent changed, without its credentials.
type RelayResult struct {
	Paths []string
	Notes []string
	// Pending means the configuration is written but needs a developer's action.
	Pending bool
}

// RelayExporter is an agent that can export its telemetry to the local relay.
type RelayExporter interface {
	Agent
	ConfigureRelay(ctx context.Context, cfg RelayConfig) (RelayResult, error)
	// RelayPointed reports whether the agent's exporter sends to addr; known is false
	// when the agent cannot tell.
	RelayPointed(addr string) (pointed, known bool)
}

// RelayChecker is an agent with a condition that keeps its telemetry from the relay
// after setup.
type RelayChecker interface {
	RelayProblem(stateDir string) (detail, fix string, ok bool)
}

// Labeled is an agent whose hooks and trailers use a label other than its name.
type Labeled interface {
	Tool() string
}

// Tool is the label a's hooks, claims and trailers carry.
func Tool(a Agent) string {
	if l, ok := a.(Labeled); ok {
		return l.Tool()
	}
	return a.Name()
}

// Exporting is an agent whose OTLP exporter terma configures in the agent's own settings.
type Exporting interface {
	Agent
	Harness() harness.Harness
}
