package agents

import (
	"context"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// UserHooks is an agent whose machine-wide hooks setup writes.
type UserHooks interface {
	Agent
	UserHooksPath() (string, error)
	PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error)
}

// UserHooksTrust is an agent that runs its machine-wide hooks only once the developer
// trusts them; UserHooksTrustStep says how, and UserHooksTrusted whether terma's entries are
// there (present) and run as written (trusted).
type UserHooksTrust interface {
	UserHooksTrustStep() string
	UserHooksTrusted() (present, trusted bool, err error)
}

// HookTrusting is an agent whose approvals terma keeps in step with the hooks it writes:
// approved for each entry of hooksFile byte for byte terma's (written with command),
// withdrawn once terma removes it. Other entries stay untouched.
type HookTrusting interface {
	SyncHookTrust(hooksFile string, command func(event string) string) (TrustSync, error)
}

// TrustSync is what a SyncHookTrust changed.
type TrustSync struct{ Approved, Withdrawn int }

// ManagedHooks is an agent whose machine-wide hooks an organization can deploy.
type ManagedHooks interface {
	Agent
	ManagedHookFiles(root string) []string
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

// Renderer is an agent with hooks that render another command's output; they run even
// with hooks switched off (env.Spool is nil) and never claim a session.
type Renderer interface {
	Renders() map[string]RenderHandler
}

// OffSwitched is an agent with events that run something else when hooks are switched off.
type OffSwitched interface {
	WhenHooksOff() map[string]Handler
}
