package cmd

import (
	"context"
	"fmt"

	"github.com/miradorlabs/terma-cli/internal/compat"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

func doctorCodexCompatibility(ctx context.Context) doctor.Check {
	path, err := shim.RealBinary(shim.AgentCodex)
	if err != nil {
		return doctor.Check{Status: doctor.Skip, Inconclusive: true, Detail: "could not locate the real Codex CLI executable"}
	}
	return codexCompatibilityCheck(compat.Resolve(ctx, compat.Installation{Harness: shim.AgentCodex, Surface: compat.CLI, Path: path}))
}

func codexCompatibilityCheck(p compat.Profile) doctor.Check {
	version := p.Installation.Version
	if version == "" {
		version = "version unknown"
	}
	capability := p.Capability(compat.CodexNoDaemon)
	behavior := "--no-daemon omitted"
	if capability.Support == compat.Supported {
		behavior = "--no-daemon enabled for interactive launches"
	}
	c := doctor.Check{Status: doctor.Pass, Detail: fmt.Sprintf("Codex CLI %s: %s (%s)", output.SanitizeTerminal(version), behavior, capability.Source)}
	if capability.Support == compat.Unknown {
		c.Status, c.Inconclusive = doctor.Skip, true
		c.Detail += "; " + capability.Reason
	}
	return c
}
