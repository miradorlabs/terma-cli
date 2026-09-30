// Package exporter configures agents to send to the local relay. Each integration
// owns its exporter configuration; commands only select and invoke integrations.
package exporter

import (
	"context"
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Config is the machine-local destination and command used by relay exporters.
// Token must never be printed or passed to an agent's tool environment.
type Config struct {
	Endpoint    string
	Token       string
	HookCommand []string
	StateDir    string
}

// Result describes configuration changes without exposing exporter credentials.
type Result struct {
	Paths   []string
	Notes   []string
	Pending bool // configuration exists but requires a developer action
}

// Exporter is an agent's ability to configure local relay export. This is separate
// from harness.Harness: an extension-only agent has no direct telemetry connection.
type Exporter interface {
	Name() string
	DisplayName() string
	Tool() string
	Selections() []string
	Configure(context.Context, Config) (Result, error)
}

var registry = []Exporter{
	claude{native{harness.Claude{}}},
	codex{native{harness.Codex{}}},
	native{harness.OpenCode{}},
	extension{"omp", "omp", configureOmp},
	extension{"pi", "Pi", configurePi},
	extension{"hermes", "Hermes", configureHermes},
	extension{"gemini", "Gemini CLI", configureGemini},
	extension{"dsh", "DeepSeek Harness", configureDsh},
}

// All returns relay exporters in configuration order.
func All() []Exporter { return slices.Clone(registry) }

// Names returns the accepted relay exporter names.
func Names() []string {
	var names []string
	for _, e := range registry {
		names = append(names, e.Name())
	}
	return names
}

// Lookup resolves a relay exporter without configuring it.
func Lookup(name string) (Exporter, error) {
	for _, e := range registry {
		if e.Name() == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("unknown relay exporter %q (choose %v)", name, Names())
}

// Targets normalizes selected surfaces to exporters, retaining registry order.
func Targets(selected []string) []string {
	var names []string
	for _, e := range registry {
		for _, choice := range e.Selections() {
			if slices.Contains(selected, choice) {
				names = append(names, e.Name())
				break
			}
		}
	}
	return names
}

// NameForTool resolves the hook label used to select a project's exporter key.
func NameForTool(tool string) string {
	for _, e := range registry {
		if e.Tool() == tool {
			return e.Name()
		}
	}
	return tool
}
