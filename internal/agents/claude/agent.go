// Package claude integrates Claude Code, and Claude Desktop through the same settings.
package claude

import (
	"context"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent is Claude Code: project hooks in .claude/settings.json, wired into every
// repository by default because the file is Claude Code's own project settings and a
// repository without one loses nothing by gaining it.
type Agent struct{}

func (Agent) Name() string                       { return "claude" }
func (Agent) DisplayName() string                { return "Claude Code" }
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
		"post-tool-use": postToolUse,
		"stop":          stop,
		"stop-failure":  stopFailure,
		// Turn start: claims the session for the local relay and starts it (internal/cli/hook.go
		// does both from the payload); the handler itself only reads the payload.
		"user-prompt-submit": hookrun.TurnStart,
		// A subagent runs inside the session; both are notification-only for terma.
		"subagent-start": subagentStart,
		"subagent-stop":  subagentStop,
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

// Renders is the status line: Claude Code's statusLine command once terma has wrapped
// it. The renderer has its own deadline even when Claude does not cancel it; capture
// starts detached delivery before waiting for rendering.
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

// InstallStatusLine wraps the status line.
func (Agent) InstallStatusLine() (bool, error) { return exporter{}.InstallStatusLine() }

// StatusLineState reports the status line as terma sees it from cwd.
func (Agent) StatusLineState(cwd string) (agents.StatusLineState, error) {
	return exporter{}.StatusLineState(cwd)
}

// RemoveStatusLine puts back the status line terma wrapped.
func (Agent) RemoveStatusLine() (bool, error) { return exporter{}.RemoveStatusLine() }

// EmissionStatus reads the user and repository settings together.
func (Agent) EmissionStatus(root string) (harness.Status, error) {
	return exporter{}.EmissionStatus(root)
}

// TelemetrySwitch is Claude Code's master switch.
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
