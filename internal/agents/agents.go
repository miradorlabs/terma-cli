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

// Selector is an agent a developer can select under more than one name (Codex CLI and
// Codex Desktop are both Codex).
type Selector interface {
	Selections() []string
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
	ManagedHookFiles(root string) []string
}

// Selections is every name a developer may select a under, its own first.
func Selections(a Agent) []string {
	if s, ok := a.(Selector); ok {
		return s.Selections()
	}
	return []string{a.Name()}
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
