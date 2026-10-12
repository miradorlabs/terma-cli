// Package codex integrates Codex, the CLI and Desktop.
package codex

import (
	"context"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// name keys the keystore too.
const (
	name        = "codex"
	displayName = "Codex"
)

// Agent covers Codex's machine-wide hooks and its user-level notifier.
type Agent struct {
	// ConfigDir is terma's config directory, where it records what it wrote to Codex's config.
	ConfigDir string
}

// Name is the agent's token.
func (Agent) Name() string { return name }

// DisplayName is how prose names the agent.
func (Agent) DisplayName() string { return displayName }

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"codex-notify":             notifyHook,
		"codex-session-start":      sessionStart,
		"codex-user-prompt-submit": userPromptSubmit,
		"codex-permission-request": permissionRequest,
		"codex-session-end":        sessionEnd,
		"codex-pre-tool-use":       preToolUse,
		"codex-post-tool-use":      postToolUse,
		"codex-stop":               stop,
		"codex-subagent-start":     subagentStart,
		"codex-subagent-stop":      subagentStop,
	}
}

func (Agent) FlushAfter() []string {
	return []string{"codex-notify", "codex-session-end", "codex-stop"}
}

func (Agent) UserHooksPath() (string, error) { return userHooksPath() }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}
func (Agent) ManagedHookFiles(root string) []string {
	return []string{filepath.Join(root, "etc", "codex", "requirements.toml")}
}

// ManagedConfig is a requirements.toml of terma's hooks, which Codex runs with no trust step.
func (Agent) ManagedConfig(command func(string) string) (string, []byte, error) {
	return "codex-requirements.toml", []byte(managedRequirements(command)), nil
}

// ManagedDeploy says where requirements.toml goes.
func (Agent) ManagedDeploy() string {
	return "`/etc/codex/requirements.toml` (append to one you already deploy), or\n  the same table in your MDM profile for `com.openai.codex`."
}

// SyncHookTrust approves, in Codex's config, the entries of hooksFile that are byte for
// byte terma's, and withdraws the approvals terma wrote for entries no longer there.
func (a Agent) SyncHookTrust(hooksFile string, command func(event string) string) (agents.TrustSync, error) {
	configPath, err := (exporter{}).ConfigPath()
	if err != nil {
		return agents.TrustSync{}, err
	}
	done, err := syncHookTrust(a.ConfigDir, configPath, hooksFile, command)
	return agents.TrustSync{Approved: done.Approved, Withdrawn: done.Withdrawn}, err
}

// Harness is how terma configures the agent's exporter.
func (a Agent) Harness() harness.Harness { return exporter{dir: a.ConfigDir} }

// WhenHooksOff runs the developer's own notifier: terma replaced Codex's direct notify
// invocation, so it must still fire with capture off.
func (Agent) WhenHooksOff() map[string]agents.Handler {
	return map[string]agents.Handler{"codex-notify": func(ctx context.Context, env hookrun.Env) error {
		if len(env.Args) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return runPreviousNotify(ctx, env.ConfigDir, env.Args[0])
	}}
}

// ContentConsented is the consent replies and thread names travel under.
func (Agent) ContentConsented(c hookrun.Consent) bool { return repliesConsented(c) }

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "export is machine-wide; it cannot be scoped to a repository"}
}

// UserHooksTrustStep is how the developer trusts the machine-wide hooks.
func (Agent) UserHooksTrustStep() string {
	return "Approve Terma's hooks in Codex: run `/hooks` (in the desktop app: Settings → Hooks → Review). Until then Codex runs none of the new or changed ones, and records nothing for them."
}

// UserHooksTrusted reports whether Codex runs every one of terma's machine-wide entries as
// written: one changed since it was trusted, or switched off, is skipped in silence.
func (Agent) UserHooksTrusted() (present, trusted bool, err error) {
	path, err := userHooksPath()
	if err != nil {
		return false, false, err
	}
	entries, err := termaEntriesIn(path)
	if err != nil || len(entries) == 0 {
		return false, false, err
	}
	trust, err := (exporter{}).hookTrustFor(path)
	if err != nil {
		return true, false, err
	}
	if trust.Disabled > 0 {
		return true, false, nil
	}
	for _, e := range entries {
		if trust.TrustedHashes[e.Key()] != e.Hash {
			return true, false, nil
		}
	}
	return true, true, nil
}

// RemoveNotifier restores the user's own notifier.
func (a Agent) RemoveNotifier() (bool, error) { return exporter{dir: a.ConfigDir}.RemoveNotifier() }

var (
	_ agents.Notifier       = Agent{}
	_ agents.StateKeeper    = Agent{}
	_ agents.UserHooksTrust = Agent{}
	_ agents.Covered        = Agent{}
	_ agents.ContentConsent = Agent{}
	_ agents.OffSwitched    = Agent{}
	_ agents.Exporting      = Agent{}
	_ agents.Agent          = Agent{}
	_ agents.HookTrusting   = Agent{}
	_ agents.UserHooks      = Agent{}
	_ agents.ManagedHooks   = Agent{}
	_ agents.Surfaced       = Agent{}
)

// userHooksPath is the user hook file shared by Codex's CLI and Desktop.
func userHooksPath() (string, error) {
	path, err := (exporter{}).ConfigPath()
	return filepath.Join(filepath.Dir(path), "hooks.json"), err
}

// StateDirs are the directories its hooks keep state in.
func (Agent) StateDirs() []string {
	return []string{codexFundingCursorDir, codexReplyCursorDir, codexDesktopCursorDir, codexTitleStateDir, codexExpectedDir}
}
