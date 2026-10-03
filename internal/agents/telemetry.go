package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
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
	// Pending means the configuration is written but needs the developer's action.
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

// MachineRefresher is an agent with home-directory files `terma update --refresh` rewrites
// to this build's templates; it rewrites only a file terma wrote, never creates one.
type MachineRefresher interface {
	Agent
	RefreshMachine() (path string, changed bool, err error)
}

// ContentConsent is an agent whose hooks spool what was said (a reply, a thread's name)
// under a consent of its own beyond the prompt policy, checked again at delivery.
type ContentConsent interface {
	ContentConsented(hookrun.Consent) bool
}

// StatusLiner is an agent whose user-level status line terma wraps to capture the plan's
// usage windows; machine-level, never repository policy.
type StatusLiner interface {
	Agent
	InstallStatusLine() (bool, error)
	StatusLineState(cwd string) (StatusLineState, error)
	RemoveStatusLine() (bool, error)
}

// StatusLineState is what the agent's status line looks like to terma.
type StatusLineState struct {
	ConfigPath string
	Installed  bool
	// Renderer is the previous command terma passes through ("" when none).
	Renderer string
	// Replaced means terma wrapped this file once and the developer has since replaced it.
	Replaced bool
	// Overrides lists settings files that outrank this one with a status line of their own.
	Overrides []string
}

// Notifier is an agent whose end-of-turn notifier an earlier terma chained in front of
// the developer's own; teardown restores theirs. Funding evidence now comes from hooks.
type Notifier interface {
	Agent
	RemoveNotifier() (bool, error)
}

// EmissionChecker is an agent whose export switches combine several settings files, so
// what a repository exports is none of them alone.
type EmissionChecker interface {
	Agent
	EmissionStatus(root string) (harness.Status, error)
	// TelemetrySwitch is the setting that turns the whole export on.
	TelemetrySwitch() string
}
