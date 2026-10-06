package agents

import (
	"context"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Handler is one `terma hook <event>` entry point.
type Handler = func(context.Context, hookrun.Env) error

// Agent is a coding agent terma integrates with.
type Agent interface {
	// Name is the token --adapters and --harness accept.
	Name() string
	DisplayName() string
	Installed(ctx context.Context) bool
	Events() map[string]Handler
	// FlushAfter lists the events that start a detached spool flush.
	FlushAfter() []string
}

// Selections is every name a developer may select a by, its own first.
func Selections(a Agent) []string {
	var out []string
	for _, s := range Surfaces(a) {
		out = append(out, s.Name)
	}
	return out
}

// DisplayNames lists agents' display names for prose.
func DisplayNames[A interface{ DisplayName() string }](as []A) string {
	names := make([]string, 0, len(as))
	for _, a := range as {
		names = append(names, a.DisplayName())
	}
	return strings.Join(names, ", ")
}
