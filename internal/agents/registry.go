package agents

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Registry is the agents a build knows, in install order; only supported ones are
// offered by setup and install, though every known agent's hooks still run.
type Registry struct {
	all       []Agent
	supported map[string]bool
	upcoming  []string

	indexOnce sync.Once
	events    map[string]Event
	renders   map[string]RenderHandler
	offs      map[string]Handler
}

// Event is one `terma hook <event>` name: the handler, the agent that owns it, and
// whether a spool flush follows it.
type Event struct {
	Handler Handler
	Agent   Agent
	Flush   bool
}

// index maps every event to its owner once, on first use, so a git hook never pays for
// it. Two agents claiming one event is a build error, never a merge of the two.
func (r *Registry) index() {
	r.indexOnce.Do(func() {
		r.events, r.renders, r.offs = map[string]Event{}, map[string]RenderHandler{}, map[string]Handler{}
		for _, a := range r.all {
			for name, h := range a.Events() {
				if other, dup := r.events[name]; dup {
					panic(fmt.Sprintf("agents: %s and %s both handle %q", other.Agent.Name(), a.Name(), name))
				}
				r.events[name] = Event{Handler: h, Agent: a, Flush: slices.Contains(a.FlushAfter(), name)}
			}
			if rd, ok := a.(Renderer); ok {
				maps.Copy(r.renders, rd.Renders())
			}
			if o, ok := a.(OffSwitched); ok {
				maps.Copy(r.offs, o.WhenHooksOff())
			}
		}
	})
}

// Event resolves a hook event to its handler and owner.
func (r *Registry) Event(name string) (Event, bool) {
	r.index()
	e, ok := r.events[name]
	return e, ok
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
	return first(r.all, func(a Agent) bool { return slices.Contains(Selections(a), selection) })
}

// Surface resolves a surface by name, with its agent.
func (r *Registry) Surface(name string) (Surface, Agent, bool) {
	for _, a := range r.all {
		for _, s := range Surfaces(a) {
			if s.Name == name {
				return s, a, true
			}
		}
	}
	return Surface{}, nil, false
}

// Names lists every known agent's name.
func (r *Registry) Names() []string { return names(r.all) }

// Handlers is every known agent's events. Names are unique across agents.
func (r *Registry) Handlers() map[string]Handler {
	r.index()
	out := make(map[string]Handler, len(r.events))
	for name, e := range r.events {
		out[name] = e.Handler
	}
	return out
}

// FlushesAfter reports whether a spool flush follows the event.
func (r *Registry) FlushesAfter(event string) bool {
	e, ok := r.Event(event)
	return ok && e.Flush
}

// ForTool resolves the agent whose hooks carry label.
func (r *Registry) ForTool(label string) (Agent, bool) {
	return first(r.all, func(a Agent) bool { return Tool(a) == label })
}

// Render is the render hook for event, when an agent has one.
func (r *Registry) Render(event string) (RenderHandler, bool) {
	r.index()
	h, ok := r.renders[event]
	return h, ok
}

// WhenHooksOff is what event runs while hooks are switched off, when anything does.
func (r *Registry) WhenHooksOff(event string) (Handler, bool) {
	r.index()
	h, ok := r.offs[event]
	return h, ok
}

// PayloadSession reads a hook payload's session the way the agent that owns event writes it.
func (r *Registry) PayloadSession(event string, payload []byte) (hookrun.PayloadSession, bool) {
	if e, ok := r.Event(event); ok {
		if pr, ok := e.Agent.(PayloadReader); ok {
			return pr.PayloadSession(payload)
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
	return collect(r.With[RelayExporter](), func(e RelayExporter) (string, bool) {
		return e.Name(), slices.ContainsFunc(Selections(e), func(s string) bool { return slices.Contains(selected, s) })
	})
}

// Harnesses is every exporting agent's harness, in registry order.
func (r *Registry) Harnesses() []harness.Harness {
	return collect(r.With[Exporting](), func(e Exporting) (harness.Harness, bool) { return e.Harness(), true })
}

// With returns the known agents that have capability C.
func (r *Registry) With[C any]() []C {
	return collect(r.all, func(a Agent) (C, bool) { c, ok := a.(C); return c, ok })
}

// Find resolves an agent by name when it has capability C.
func (r *Registry) Find[C any](name string) (C, bool) {
	a, ok := first(r.all, func(a Agent) bool { _, ok := a.(C); return ok && a.Name() == name })
	if !ok {
		var zero C
		return zero, false
	}
	return a.(C), true
}

// collect keeps f's result for each x it reports.
func collect[T, R any](xs []T, f func(T) (R, bool)) []R {
	var out []R
	for _, x := range xs {
		if r, ok := f(x); ok {
			out = append(out, r)
		}
	}
	return out
}

// first is the first x that match reports.
func first[T any](xs []T, match func(T) bool) (T, bool) {
	if i := slices.IndexFunc(xs, match); i >= 0 {
		return xs[i], true
	}
	var zero T
	return zero, false
}

// names lists the agents' names.
func names[A interface{ Name() string }](as []A) []string {
	return collect(as, func(a A) (string, bool) { return a.Name(), true })
}
