// Package dsh integrates DeepSeek Harness.
package dsh

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// Agent is DeepSeek Harness, whose events come from terma's user-level Cordis plugin.
type Agent struct{ relayexport.Own }

func (Agent) Name() string        { return "dsh" }
func (Agent) DisplayName() string { return "DeepSeek Harness" }
func (Agent) Installed(context.Context) bool {
	_, err := exec.LookPath("dsh")
	return err == nil
}

func (Agent) Events() map[string]agents.Handler { return hookrun.Extension{Tool: "dsh"}.Events("dsh") }

func (Agent) FlushAfter() []string { return []string{"dsh-session-end"} }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportPartial, Note: "dsh's own export goes to DeepSeek; terma's plugin exports tokens (auxiliary calls included) and tool calls through the local relay only; no cost"}
}

var (
	_ agents.Covered = Agent{}
	_ agents.Agent   = Agent{}
)
