package agents

import (
	"context"
	"strings"

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

// Selections is every name a developer may select a under, its own first.
func Selections(a Agent) []string {
	var out []string
	for _, s := range Surfaces(a) {
		out = append(out, s.Name)
	}
	return out
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

// DisplayNames lists agents' display names for prose.
func DisplayNames[A interface{ DisplayName() string }](as []A) string {
	names := make([]string, 0, len(as))
	for _, a := range as {
		names = append(names, a.DisplayName())
	}
	return strings.Join(names, ", ")
}
