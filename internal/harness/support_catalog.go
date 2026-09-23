package harness

import "strings"

// This file describes, statically, how completely terma supports each coding agent.
// It is deliberately broader than the telemetry registry (All): a harness is listed in
// the registry only when it can export OTLP, but terma also stamps commits for agents
// without a configurable client OTLP exporter, such as Cursor and Antigravity. `terma harness list`
// reports this catalog so a user can see, before wiring anything, which agents work and
// which work only in part.

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
	// Name is the command-line token (matches a telemetry harness name where one exists).
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

// overall folds a set of capabilities into a single level: full when all are full,
// none when none is anything but none, partial in between.
func overall(caps ...CapabilitySupport) SupportLevel {
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

// SupportCatalog is every coding agent terma integrates with, in the order they are
// listed. The overall level is computed here so a hand-edited literal can never drift
// out of step with the capabilities above it.
func SupportCatalog() []AgentSupport {
	cat := []AgentSupport{
		{
			Name:        "claude",
			DisplayName: "Claude Code",
			Attribution: CapabilitySupport{Level: SupportFull},
			Telemetry:   CapabilitySupport{Level: SupportFull},
		},
		{
			Name:        "codex",
			DisplayName: "Codex",
			Attribution: CapabilitySupport{Level: SupportFull},
			Telemetry:   CapabilitySupport{Level: SupportFull, Note: "export is machine-wide; it cannot be scoped to a repository"},
		},
		{
			Name:        "opencode",
			DisplayName: "OpenCode",
			Attribution: CapabilitySupport{Level: SupportFull},
			Telemetry:   CapabilitySupport{Level: SupportFull},
		},
		{
			Name:        "cursor",
			DisplayName: "Cursor",
			Attribution: CapabilitySupport{Level: SupportFull},
			Telemetry:   CapabilitySupport{Level: SupportPartial, Note: "ordered hook observations, every tool call and optional token snapshots; plan, quota and billed cost unavailable; backend mapping of tool calls pending"},
		},
		{
			Name:        "antigravity",
			DisplayName: "Antigravity",
			Attribution: CapabilitySupport{Level: SupportFull},
			Telemetry:   CapabilitySupport{Level: SupportPartial, Note: "hooks only: model, turns, every tool step and termination; no token counts, plan, quota, cost or tool durations — agy has no OTLP export; backend mapping of tool calls pending"},
		},
	}
	for i := range cat {
		cat[i].Support = overall(cat[i].Attribution, cat[i].Telemetry)
	}
	return cat
}

// SupportNames lists the tokens the catalog accepts, for usage strings and error text.
func SupportNames() []string {
	cat := SupportCatalog()
	out := make([]string, 0, len(cat))
	for _, a := range cat {
		out = append(out, a.Name)
	}
	return out
}

// LookupSupport resolves a command-line token to its catalog entry.
func LookupSupport(name string) (AgentSupport, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, a := range SupportCatalog() {
		if a.Name == want {
			return a, true
		}
	}
	return AgentSupport{}, false
}
