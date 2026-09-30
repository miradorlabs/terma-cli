package agents

import (
	"slices"
	"strings"
)

// SupportLevel is how completely terma supports a capability, or an agent overall.
type SupportLevel string

const (
	// SupportFull means the capability works with no missing functionality.
	SupportFull SupportLevel = "full"
	// SupportPartial is an agent that has at least one capability but not all of them.
	SupportPartial SupportLevel = "partial"
	// SupportNone is a capability terma cannot provide for this agent.
	SupportNone SupportLevel = "none"
)

// CapabilitySupport is terma's support for one capability of one agent. Note carries a
// short caveat and is expected to be set whenever the level is anything but full, and
// may also flag a limitation that does not itself lower the level.
type CapabilitySupport struct {
	Level SupportLevel `json:"level"`
	Note  string       `json:"note,omitempty"`
}

// AgentSupport is terma's support for one coding agent, across every capability.
type AgentSupport struct {
	// Name is the agent's command-line token.
	Name string `json:"name"`
	// DisplayName is how the agent is written in prose — "Claude Code".
	DisplayName string `json:"display_name"`
	// Attribution is stamping commits with the session and tool that produced them,
	// via committed hooks (or, for OpenCode, a plugin).
	Attribution CapabilitySupport `json:"attribution"`
	// Telemetry is exporting OTLP usage to Terma so `usage` and `session` can report
	// spend and cost.
	Telemetry CapabilitySupport `json:"telemetry"`
	// Support is the overall level: full only when every capability is full, none when
	// none of them is, partial otherwise.
	Support SupportLevel `json:"support"`
}

// Overall folds a set of capabilities into a single level: full when all are full,
// none when none is anything but none, partial in between.
func Overall(caps ...CapabilitySupport) SupportLevel {
	full, present := 0, 0
	for _, c := range caps {
		if c.Level == SupportFull {
			full++
		}
		if c.Level != SupportNone {
			present++
		}
	}
	switch {
	case full == len(caps):
		return SupportFull
	case present == 0:
		return SupportNone
	default:
		return SupportPartial
	}
}

// Covered is an agent that says how completely terma supports it, for `terma harness
// list`.
type Covered interface {
	Agent
	Coverage() (attribution, telemetry CapabilitySupport)
}

// SupportCatalog is every agent that declares its coverage, in registry order, with the
// overall level folded from its capabilities so it can never drift from them.
func (r *Registry) SupportCatalog() []AgentSupport {
	var out []AgentSupport
	for _, a := range r.With[Covered]() {
		attribution, telemetry := a.Coverage()
		out = append(out, AgentSupport{Name: a.Name(), DisplayName: a.DisplayName(), Attribution: attribution,
			Telemetry: telemetry, Support: Overall(attribution, telemetry)})
	}
	return out
}

// SupportNames lists the tokens the catalog accepts.
func (r *Registry) SupportNames() []string {
	var out []string
	for _, a := range r.SupportCatalog() {
		out = append(out, a.Name)
	}
	return out
}

// LookupSupport resolves a command-line token to its catalog entry.
func (r *Registry) LookupSupport(name string) (AgentSupport, bool) {
	cat := r.SupportCatalog()
	i := slices.IndexFunc(cat, func(a AgentSupport) bool { return a.Name == strings.ToLower(strings.TrimSpace(name)) })
	if i < 0 {
		return AgentSupport{}, false
	}
	return cat[i], true
}
