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

// MachineRefresher is an agent with home-directory files terma rewrites to this build's
// templates on `terma update --refresh`. It rewrites only a file terma wrote, never
// creates one, and reports the path it changed.
type MachineRefresher interface {
	Agent
	RefreshMachine() (path string, changed bool, err error)
}

// ContentConsent is an agent whose hooks spool what was said (a reply, a thread's name),
// under a consent of its own beyond the organization's prompt policy, checked again at
// every delivery.
type ContentConsent interface {
	ContentConsented(projectID string, global bool) bool
}

// StatusLiner is an agent whose user-level status line terma wraps to capture the
// plan's usage windows. It is machine-level capture, never repository policy.
type StatusLiner interface {
	Agent
	InstallStatusLine() (bool, error)
	StatusLineState(cwd string) (StatusLineState, error)
	RemoveStatusLine() (bool, error)
}

// StatusLineState is what the agent's status line looks like to terma.
type StatusLineState struct {
	ConfigPath string
	// Installed: the file holds terma's command.
	Installed bool
	// Renderer is the previous command terma passes through ("" when none).
	Renderer string
	// Replaced: terma wrapped this file once, and the file now holds a status line that
	// is not terma's. Capture has stopped; the developer's entry stands.
	Replaced bool
	// Overrides lists settings files that outrank this one and define their own status
	// line, so terma's never runs there.
	Overrides []string
}

// Notifier is an agent with an end-of-turn notifier terma chains in front of the
// developer's own, to capture funding evidence.
type Notifier interface {
	Agent
	NotifierInstalled() (bool, error)
	InstallNotifier() (bool, error)
	RemoveNotifier() (bool, error)
}

// EmissionChecker is an agent whose export switches combine several settings files, so
// what a repository exports is none of them alone.
type EmissionChecker interface {
	Agent
	// EmissionStatus is the combined export for the repository at root.
	EmissionStatus(root string) (harness.Status, error)
	// TelemetrySwitch is the setting that turns the whole export on.
	TelemetrySwitch() string
}
