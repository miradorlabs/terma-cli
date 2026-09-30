// Package agents is the contract a coding agent implements and the registry of the
// agents a build of terma knows. Each agent lives in its own package below this one;
// package builtin registers them.
package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Handler is one `terma hook <event>` entry point.
type Handler = func(context.Context, hookrun.Env) error

// Agent is a coding agent terma integrates with.
type Agent interface {
	// Name is the token --adapters and --harness accept.
	Name() string
	DisplayName() string
	Installed(ctx context.Context) bool
	// HooksPath is the committed hooks file, relative to the repository root, or ""
	// for an agent whose hooks are user-scope.
	HooksPath() string
	// Default reports whether a plain `terma install` wires this agent in root.
	Default(root string) bool
	Plan(root string, install bool) (hookmgr.Plan, error)
	// Events maps each committed `terma hook <event>` name to its handler.
	Events() map[string]Handler
	// FlushAfter lists the events that start a detached spool flush.
	FlushAfter() []string
}

// Surface is one way a developer runs an agent, chosen on its own at setup: Codex's CLI
// and its desktop app are two.
type Surface struct {
	Name, DisplayName string
	Installed         func(context.Context) bool
	// Needs is what an install must give the surface for it to report at all.
	Needs Needs
	// InstallSteps and SetupSteps are what the developer does next for it to report.
	InstallSteps, SetupSteps []string
	// Reports says, after an install, how the surface's sessions reach Terma.
	Reports string
	// Warn is a condition on this machine the developer should know about, continuing a
	// sentence that starts with the agent's name; "" when there is none.
	Warn func() string
}

// Needs is what a surface cannot report without.
type Needs struct {
	Signals []string
	// Hooks: the agent's committed hooks, wired and applied.
	Hooks bool
}

// Surfaced is an agent run as more than one surface.
type Surfaced interface {
	Surfaces() []Surface
}

// TrustState is whether an agent will run the hooks a repository commits. Detail
// continues a sentence ending in "<agent> hooks present"; Fix applies when not Trusted.
type TrustState struct {
	Trusted bool
	Detail  string
	Fix     string
}

// Trusting is an agent that runs committed hooks only once the developer trusts them.
type Trusting interface {
	Agent
	Trust(root string) (TrustState, error)
}

// UserHooks is an agent whose machine-wide hooks global mode writes.
type UserHooks interface {
	Agent
	UserHooksPath() (string, error)
	PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error)
}

// ManagedHooks is an agent whose machine-wide hooks an organization can deploy.
type ManagedHooks interface {
	Agent
	// ManagedHookFiles are where an administrator deploys them, under root.
	ManagedHookFiles(root string) []string
	// ManagedConfig is the file an organization deploys, by the name it is written as.
	ManagedConfig(command func(event string) string) (name string, data []byte, err error)
	// ManagedDeploy says where that file goes, as Markdown.
	ManagedDeploy() string
}

// Selections is every name a developer may select a under, its own first.
func Selections(a Agent) []string {
	var out []string
	for _, s := range Surfaces(a) {
		out = append(out, s.Name)
	}
	return out
}

// Surfaces is a's surfaces: its own, or the agent itself as its one.
func Surfaces(a Agent) []Surface {
	if s, ok := a.(Surfaced); ok {
		return s.Surfaces()
	}
	return []Surface{{Name: a.Name(), DisplayName: a.DisplayName(), Installed: a.Installed}}
}

// Wired reports whether root's committed hooks file carries a's entries. A file that
// cannot be read counts as wired, so the plan built from it raises the error.
func Wired(root string, a Agent) bool {
	if a.HooksPath() == "" {
		return false
	}
	plan, err := a.Plan(root, false)
	return err != nil || !plan.Empty()
}

// PayloadReader is an agent whose hook payloads name their session in keys of their own;
// the others' are read by hookrun.ReadPayloadSession.
type PayloadReader interface {
	PayloadSession(payload []byte) (hookrun.PayloadSession, bool)
}

// MachineRefresher is an agent with home-directory files terma rewrites to this build's
// templates on `terma update --refresh`. It rewrites only a file terma wrote, never
// creates one, and reports the path it changed.
type MachineRefresher interface {
	Agent
	RefreshMachine() (path string, changed bool, err error)
}

// RenderHandler is a hook that draws something; its result is the process's exit status.
type RenderHandler = func(context.Context, hookrun.Env) int

// Renderer is an agent with hooks that render another command's output. They run even
// with hooks switched off, capturing nothing (env.Spool is nil), never claim a session,
// and end the process with their own exit status.
type Renderer interface {
	Renders() map[string]RenderHandler
}

// OffSwitched is an agent with events that still owe the developer something when hooks
// are switched off: what they run instead.
type OffSwitched interface {
	WhenHooksOff() map[string]Handler
}
