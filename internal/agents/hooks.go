package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

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

// UserHooksTrust is an agent that runs its machine-wide hooks only once the developer
// trusts them; UserHooksTrustStep says how.
type UserHooksTrust interface {
	UserHooksTrustStep() string
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

// PayloadReader is an agent whose hook payloads name their session in keys of their own;
// the others' are read by hookrun.ReadPayloadSession.
type PayloadReader interface {
	PayloadSession(payload []byte) (hookrun.PayloadSession, bool)
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

// Retrusting is an agent that runs a changed committed hook only after the developer
// trusts it again; RetrustNote says so when a refresh rewrote the file.
type Retrusting interface {
	Agent
	RetrustNote() string
}
