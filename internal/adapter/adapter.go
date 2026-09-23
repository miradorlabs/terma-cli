// Package adapter is the registry of coding-agent adapters: for each agent, what
// `terma install` writes into a repository and which `terma hook <event>` entry points
// those committed files call.
//
// It is the seam between two packages that must not know each other. hookmgr knows
// each agent's hooks file and how to merge terma's entries into it without disturbing
// anyone else's; hookrun knows what to do when one of those entries fires. Neither
// knows the list of agents. This package does, once, so that install, uninstall,
// doctor and hook dispatch iterate over the same registry instead of each naming the
// agents by hand — and adding an agent is one file here plus one line below.
//
// The telemetry registry (harness.All) is a different list on purpose: an agent is
// there only when it has a configurable OTLP exporter terma can point at Terma. Cursor
// and Antigravity have none and are absent from it, yet both are adapters here, because
// commit attribution and hook observations need no exporter.
package adapter

import (
	"context"
	"maps"
	"slices"
	"sort"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Handler is one `terma hook <event>` entry point.
type Handler = func(context.Context, hookrun.Env) error

// Adapter is terma's integration with one coding agent at repository scope.
type Adapter interface {
	// Name is the token `--adapters` accepts and .terma/settings.json records.
	Name() string
	// DisplayName is how the agent is written in prose — "Claude Code".
	DisplayName() string
	// Installed reports whether the agent is on this machine, which is what `terma
	// setup` preselects. It is the adapter's to answer: only it knows the agent's
	// binary names, and cmd used to keep a second list of them.
	Installed(ctx context.Context) bool
	// HooksPath is the committed file the adapter writes, relative to the repository
	// root, or "" for an adapter that writes nothing into a repository (OpenCode's
	// plugin is user-scope and calls the binary directly).
	HooksPath() string
	// Default reports whether a plain `terma install` wires this adapter in root: a
	// hooks file is a committed directory nobody wants in a repository no one opens
	// in that agent, so most adapters say yes only where the agent's directory
	// already exists.
	Default(root string) bool
	// Plan computes the repository changes that install (or remove) the adapter's
	// hooks. An adapter with no HooksPath returns an empty plan.
	Plan(root string, install bool) (hookmgr.Plan, error)
	// Events maps each hook event name this adapter's files call to its handler. The
	// names are committed wiring and must stay stable across versions.
	Events() map[string]Handler
	// FlushAfter lists the events that start a detached spool flush once handled:
	// the natural end-of-turn moments when new events exist and a few hundred
	// milliseconds of background work is invisible.
	FlushAfter() []string
}

// TrustState is whether an agent will actually run the hooks a repository commits.
// Some agents load a repository's hooks only after the developer has trusted the
// repository, or the hooks, once from inside the agent — which is silent from terma's
// side unless doctor goes looking. Detail continues a sentence that ends in
// "<agent> hooks present"; Fix is what to do when Trusted is false.
type TrustState struct {
	Trusted bool
	Detail  string
	Fix     string
}

// Trusting is implemented by adapters whose agent gates committed hooks behind a trust
// decision it records somewhere terma can read.
type Trusting interface {
	Adapter
	Trust(root string) (TrustState, error)
}

// registry is fixed at compile time: an adapter has to know an agent's file format and
// payload, so there is nothing a runtime registration would enable. The order is the
// order `terma install` plans and reports them.
var registry = []Adapter{claude{}, cursor{}, codex{}, opencode{}, antigravity{}}

// All returns every adapter, in registry order.
func All() []Adapter {
	out := make([]Adapter, len(registry))
	copy(out, registry)
	return out
}

// Lookup resolves an adapter name.
func Lookup(name string) (Adapter, bool) {
	for _, a := range registry {
		if a.Name() == name {
			return a, true
		}
	}
	return nil, false
}

// Names lists every adapter token.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, a := range registry {
		out = append(out, a.Name())
	}
	return out
}

// RepoNames lists the adapters that write a file into the repository — the ones worth
// offering in `--adapters` help and error text.
func RepoNames() []string {
	var out []string
	for _, a := range registry {
		if a.HooksPath() != "" {
			out = append(out, a.Name())
		}
	}
	return out
}

// Handlers is the union of every adapter's events. Names are unique across adapters
// (TestEventNamesAreUnique); a collision would silently route one agent's payload to
// another's handler.
func Handlers() map[string]Handler {
	out := map[string]Handler{}
	for _, a := range registry {
		maps.Copy(out, a.Events())
	}
	return out
}

// FlushesAfter reports whether a spool flush follows the event.
func FlushesAfter(event string) bool {
	for _, a := range registry {
		if slices.Contains(a.FlushAfter(), event) {
			return true
		}
	}
	return false
}

// EventNames lists every hook event, sorted. Nothing shipped calls it: it is what the
// contract test pins, because these names are committed into customers' repositories.
func EventNames() []string {
	var out []string
	for event := range Handlers() {
		out = append(out, event)
	}
	sort.Strings(out)
	return out
}
