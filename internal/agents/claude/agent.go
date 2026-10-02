// Package claude integrates Claude Code, and Claude Desktop through the same settings.
package claude

import (
	"context"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

const (
	name        = "claude"
	displayName = "Claude Code"
)

// Agent is Claude Code: hooks in .claude/settings.json, wired by default because a repository
// loses nothing by gaining that file.
type Agent struct{}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return settingsPath }
func (Agent) Default(string) bool                { return true }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planSettings(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"session-start": sessionStart,
		"session-end":   sessionEnd,
		"pre-tool-use":  preToolUse,
		"post-tool-use": postToolUse,
		"stop":          stop,
		"stop-failure":  stopFailure,
		// internal/cli/hook.go claims the session and starts the relay from the payload.
		"user-prompt-submit": hookrun.TurnStart,
		"subagent-start":     subagentStart,
		"subagent-stop":      subagentStop,
	}
}

func (Agent) FlushAfter() []string { return []string{"session-end", "stop", "stop-failure"} }

func (Agent) UserHooksPath() (string, error) { return (exporter{}).ConfigPath() }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}
func (Agent) ManagedHookFiles(root string) []string {
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join(root, "Library", "Application Support", "ClaudeCode", "managed-settings.json")}
	}
	return []string{filepath.Join(root, "etc", "claude-code", "managed-settings.json")}
}

// ManagedConfig is a managed-settings.json holding terma's hooks.
func (Agent) ManagedConfig(command func(string) string) (string, []byte, error) {
	data, err := managedSettings(command)
	return "claude-managed-settings.json", data, err
}

// ManagedDeploy says where managed-settings.json goes.
func (Agent) ManagedDeploy() string {
	return "macOS `/Library/Application Support/ClaudeCode/managed-settings.json`,\n  Linux `/etc/claude-code/managed-settings.json` (merge its `hooks` into a file you already deploy)."
}

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return exporter{} }

// RefreshMachine rewrites the status-line wrap.
func (Agent) RefreshMachine() (string, bool, error) { return exporter{}.RefreshStatusLine() }

// Renders is the wrapped statusLine command; the renderer has its own deadline.
func (Agent) Renders() map[string]agents.RenderHandler {
	return map[string]agents.RenderHandler{"statusline": func(ctx context.Context, env hookrun.Env) int {
		renderer, err := statusLineRenderer()
		if err != nil {
			env.Logf("status line record: %v", err)
		}
		return statusLine(ctx, env, statusLineOptions{Renderer: renderer, Indicator: env.Spool != nil, OnCapture: env.Flush})
	}}
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull}
}

// InstallStatusLine wraps the user's status line.
func (Agent) InstallStatusLine() (bool, error) { return exporter{}.InstallStatusLine() }

// StatusLineState is the status line as cwd's settings leave it.
func (Agent) StatusLineState(cwd string) (agents.StatusLineState, error) {
	return exporter{}.StatusLineState(cwd)
}

// RemoveStatusLine restores the user's own status line.
func (Agent) RemoveStatusLine() (bool, error) { return exporter{}.RemoveStatusLine() }

// EmissionStatus is what the merged settings at root export.
func (Agent) EmissionStatus(root string) (harness.Status, error) {
	return exporter{}.EmissionStatus(root)
}

// TelemetrySwitch is the setting that turns Claude Code's telemetry on.
func (Agent) TelemetrySwitch() string { return exporter{}.TelemetrySwitch() }

var (
	_ agents.StatusLiner      = Agent{}
	_ agents.EmissionChecker  = Agent{}
	_ agents.Covered          = Agent{}
	_ agents.Renderer         = Agent{}
	_ agents.MachineRefresher = Agent{}
	_ agents.Exporting        = Agent{}
	_ agents.Agent            = Agent{}
	_ agents.UserHooks        = Agent{}
	_ agents.ManagedHooks     = Agent{}
)
