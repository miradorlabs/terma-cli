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

// CapabilitySupport is terma's support for one capability of one agent; Note is a caveat,
// set whenever the level is not full.
type CapabilitySupport struct {
	Level SupportLevel `json:"level"`
	Note  string       `json:"note,omitempty"`
}

// AgentSupport is terma's support for one coding agent, across every capability.
type AgentSupport struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Attribution is stamping commits with the session and tool that produced them.
	Attribution CapabilitySupport `json:"attribution"`
	// Telemetry is exporting OTLP usage so `usage` and `session` can report spend.
	Telemetry CapabilitySupport `json:"telemetry"`
	// Support is the overall level, folded by Overall.
	Support SupportLevel `json:"support"`
}

// Overall is full when every capability is full, none when all are none, else partial.
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

// Covered is an agent that says how completely terma supports it, for `terma harness list`.
type Covered interface {
	Agent
	Coverage() (attribution, telemetry CapabilitySupport)
}

// SupportCatalog is every Covered agent, in registry order, its level folded by Overall.
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
