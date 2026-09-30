package agents

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Registry is the agents a build knows, in the order install plans them, and which of
// them it supports. A known agent's committed hooks still run, and uninstall and doctor
// still cover them; only supported agents are offered by setup and install.
type Registry struct {
	all       []Agent
	supported map[string]bool
	upcoming  []string
}

// New registers every agent the build knows.
func New(all ...Agent) *Registry {
	return &Registry{all: all, supported: map[string]bool{}}
}

// Support marks agents as supported: offered by setup and install.
func (r *Registry) Support(agents ...Agent) *Registry {
	for _, a := range agents {
		r.supported[a.Name()] = true
	}
	return r
}

// Upcoming announces agents the build has no integration for, by display name.
func (r *Registry) Upcoming(displayNames ...string) *Registry {
	r.upcoming = append(r.upcoming, displayNames...)
	return r
}

// UpcomingNames lists the announced agents' display names.
func (r *Registry) UpcomingNames() []string { return slices.Clone(r.upcoming) }

// All returns every known agent.
func (r *Registry) All() []Agent { return slices.Clone(r.all) }

// Supported returns the supported agents.
func (r *Registry) Supported() []Agent {
	return slices.DeleteFunc(r.All(), func(a Agent) bool { return !r.supported[a.Name()] })
}

// IsSupported reports whether selection names a supported agent.
func (r *Registry) IsSupported(selection string) bool {
	a, ok := r.Selected(selection)
	return ok && r.supported[a.Name()]
}

// Lookup resolves an agent by name.
func (r *Registry) Lookup(name string) (Agent, bool) { return r.Find[Agent](name) }

// Selected resolves any of the names an agent may be selected under.
func (r *Registry) Selected(selection string) (Agent, bool) {
	for _, a := range r.all {
		if slices.Contains(Selections(a), selection) {
			return a, true
		}
	}
	return nil, false
}

// Names lists every known agent's name.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.all))
	for _, a := range r.all {
		out = append(out, a.Name())
	}
	return out
}

// RepoNames lists the agents that commit a hooks file into a repository.
func (r *Registry) RepoNames() []string {
	var out []string
	for _, a := range r.all {
		if a.HooksPath() != "" {
			out = append(out, a.Name())
		}
	}
	return out
}

// WiredNames lists the agents whose committed hooks root carries.
func (r *Registry) WiredNames(root string) []string {
	var out []string
	for _, a := range r.all {
		if Wired(root, a) {
			out = append(out, a.Name())
		}
	}
	return out
}

// Handlers is every known agent's events. Names are unique across agents.
func (r *Registry) Handlers() map[string]Handler {
	out := map[string]Handler{}
	for _, a := range r.all {
		maps.Copy(out, a.Events())
	}
	return out
}

// FlushesAfter reports whether a spool flush follows the event.
func (r *Registry) FlushesAfter(event string) bool {
	return slices.ContainsFunc(r.all, func(a Agent) bool { return slices.Contains(a.FlushAfter(), event) })
}

// EventNames lists every hook event, sorted: the names committed into repositories.
func (r *Registry) EventNames() []string {
	return slices.Sorted(maps.Keys(r.Handlers()))
}

// ForTool resolves the agent whose hooks carry label.
func (r *Registry) ForTool(label string) (Agent, bool) {
	for _, a := range r.all {
		if Tool(a) == label {
			return a, true
		}
	}
	return nil, false
}

// ToolForEvent is the label of the agent whose hooks run event, or "" for a git hook.
func (r *Registry) ToolForEvent(event string) string {
	for _, a := range r.all {
		if _, ok := a.Events()[event]; ok {
			return Tool(a)
		}
	}
	return ""
}

// PayloadSession is what the payload of a hook for event says about its session, read
// the way the agent that owns event writes it.
func (r *Registry) PayloadSession(event string, payload []byte) (hookrun.PayloadSession, bool) {
	for _, a := range r.all {
		if _, ok := a.Events()[event]; ok {
			if pr, ok := a.(PayloadReader); ok {
				return pr.PayloadSession(payload)
			}
		}
	}
	return hookrun.ReadPayloadSession(payload)
}

// NameForTool is the name of the agent whose hooks carry label, or label itself.
func (r *Registry) NameForTool(label string) string {
	if a, ok := r.ForTool(label); ok {
		return a.Name()
	}
	return label
}

// RelayTargets names the relay exporters among selected, in registry order.
func (r *Registry) RelayTargets(selected []string) []string {
	var out []string
	for _, e := range r.With[RelayExporter]() {
		if slices.ContainsFunc(Selections(e), func(s string) bool { return slices.Contains(selected, s) }) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Harnesses is every exporting agent's harness, in registry order.
func (r *Registry) Harnesses() []harness.Harness {
	var out []harness.Harness
	for _, e := range r.With[Exporting]() {
		out = append(out, e.Harness())
	}
	return out
}

// HarnessNames lists the exporting agents' names.
func (r *Registry) HarnessNames() []string {
	var out []string
	for _, e := range r.With[Exporting]() {
		out = append(out, e.Name())
	}
	return out
}

// Harness resolves an exporting agent's harness by name.
func (r *Registry) Harness(name string) (harness.Harness, error) {
	if e, ok := r.Find[Exporting](strings.ToLower(strings.TrimSpace(name))); ok {
		return e.Harness(), nil
	}
	return nil, fmt.Errorf("unknown agent %q (want %s)", name, strings.Join(r.HarnessNames(), " or "))
}

// With returns the known agents that have capability C.
func (r *Registry) With[C any]() []C {
	var out []C
	for _, a := range r.all {
		if c, ok := a.(C); ok {
			out = append(out, c)
		}
	}
	return out
}

// Find resolves an agent by name when it has capability C.
func (r *Registry) Find[C any](name string) (C, bool) {
	for _, a := range r.all {
		if c, ok := a.(C); ok && a.Name() == name {
			return c, true
		}
	}
	var zero C
	return zero, false
}
