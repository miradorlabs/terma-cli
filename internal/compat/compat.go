// Package compat resolves harness versions into capabilities. It does not depend
// on launchers, hook installers, or telemetry readers, so all can share its rules.
package compat

import (
	"strings"

	"golang.org/x/mod/semver"
)

// Surface distinguishes products whose releases and capabilities evolve independently.
type Surface string

// Supported product surfaces; a desktop app must not inherit CLI version rules.
const (
	CLI     Surface = "cli"
	Desktop Surface = "desktop"
)

// Support distinguishes a verified answer from missing or inconclusive evidence.
type Support string

// Capability support outcomes. Unknown must never be treated as permission.
const (
	Unknown     Support = "unknown"
	Supported   Support = "supported"
	Unsupported Support = "unsupported"
)

// Feature is a stable capability identifier shared by producers and consumers.
type Feature string

// CodexNoDaemon controls whether an interactive CLI launch can bypass the daemon.
const CodexNoDaemon Feature = "codex.no-daemon"

// Installation identifies the harness and surface whose version is being resolved.
// Path is only needed for live probes; recorded producer versions need no binary.
type Installation struct {
	Harness string  `json:"harness"`
	Surface Surface `json:"surface"`
	Path    string  `json:"path,omitempty"`
	Version string  `json:"version,omitempty"`
}

// Capability carries an answer and the evidence or uncertainty behind it.
type Capability struct {
	Support  Support `json:"support"`
	Source   string  `json:"source"` // version rule, probe, or unknown
	Evidence string  `json:"evidence,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

// Profile is the resolved behavior for one installation or recorded producer.
type Profile struct {
	Installation Installation           `json:"installation"`
	Capabilities map[Feature]Capability `json:"capabilities"`
}

// Capability returns Unknown for any feature the profile does not establish.
func (p Profile) Capability(feature Feature) Capability {
	if c, ok := p.Capabilities[feature]; ok {
		return c
	}
	return Capability{Support: Unknown, Source: "unknown", Reason: "no verified compatibility rule"}
}

type rule struct {
	harness  string
	surface  Surface
	feature  Feature
	min, max string // inclusive, stable releases only
	support  Support
	evidence string
}

// Audited against upstream release-tagged source on 2026-09-29. Keep the upper
// bounds explicit: an unexamined future release must not silently become known.
// Introduction: https://github.com/openai/codex/pull/46088
var rules = []rule{
	{"codex", CLI, CodexNoDaemon, "0.150.0", "0.155.1", Unsupported,
		"https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/tui/src/cli.rs"},
	{"codex", CLI, CodexNoDaemon, "0.156.0", "0.157.1", Supported,
		"https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/tui/src/cli.rs"},
}

// ParseVersion accepts a complete semantic version, preserving prerelease and
// build metadata. Incomplete versions and arbitrary development labels are unknown.
func ParseVersion(raw string) (string, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	core := strings.SplitN(strings.SplitN(v, "+", 2)[0], "-", 2)[0]
	return v, len(strings.Split(core, ".")) == 3 && semver.IsValid("v"+v)
}

// ForVersion is pure: readers of recorded telemetry can pass the producer's
// version without inspecting or running the currently installed executable.
func ForVersion(installation Installation) Profile {
	p := Profile{Installation: installation, Capabilities: map[Feature]Capability{}}
	v, ok := ParseVersion(installation.Version)
	if !ok {
		return p
	}
	p.Installation.Version = v
	// Prereleases and custom builds can diverge from adjacent stable releases.
	if semver.Prerelease("v"+v) != "" || semver.Build("v"+v) != "" {
		return p
	}
	for _, r := range rules {
		if r.harness == installation.Harness && r.surface == installation.Surface &&
			semver.Compare("v"+v, "v"+r.min) >= 0 && semver.Compare("v"+v, "v"+r.max) <= 0 {
			p.Capabilities[r.feature] = Capability{Support: r.support, Source: "version rule", Evidence: r.evidence}
		}
	}
	return p
}
