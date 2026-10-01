// Package codex integrates Codex, the CLI and Desktop.
package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// name is the agent's name, in the keystore and the routing records too.
const name = "codex"

// Agent covers OpenAI's Codex CLI and Desktop repository hooks in .codex/hooks.json.
// The CLI's user-level notifier (`notify` in ~/.codex/config.toml, written by
// `terma connect codex`) reaches the same handler set through codex-notify.
type Agent struct{}

const codexHookReview = "open this repository in Codex Desktop, then go to Settings → Hooks → Review and approve the Terma entries (or run /hooks in Codex CLI)"

func (Agent) Name() string                       { return name }
func (Agent) DisplayName() string                { return "Codex" }
func (Agent) Installed(ctx context.Context) bool { return Codex{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hooksPath }
func (Agent) Default(root string) bool           { return hasConfig(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"codex-notify":             CodexNotify,
		"codex-session-start":      CodexSessionStart,
		"codex-user-prompt-submit": CodexUserPromptSubmit,
		"codex-pre-tool-use":       CodexPreToolUse,
		"codex-permission-request": CodexPermissionRequest,
		"codex-session-end":        CodexSessionEnd,
		"codex-post-tool-use":      CodexPostToolUse,
		"codex-stop":               CodexStop,
		"codex-subagent-start":     CodexSubagentStart,
		"codex-subagent-stop":      CodexSubagentStop,
	}
}

func (Agent) FlushAfter() []string {
	return []string{"codex-notify", "codex-session-end", "codex-stop", "codex-user-prompt-submit"}
}

func (Agent) UserHooksPath() (string, error) { return userHooksPath() }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return planUserHooks(dir, command, install)
}
func (Agent) ManagedHookFiles(root string) []string {
	return []string{filepath.Join(root, "etc", "codex", "requirements.toml")}
}

// ManagedConfig is a requirements.toml holding terma's hooks, which Codex runs with no
// trust step.
func (Agent) ManagedConfig(command func(string) string) (string, []byte, error) {
	return "codex-requirements.toml", []byte(managedRequirements(command)), nil
}

// ManagedDeploy says where requirements.toml goes.
func (Agent) ManagedDeploy() string {
	return "`/etc/codex/requirements.toml` (append to one you already deploy), or\n  the same table in your MDM profile for `com.openai.codex`."
}

// Trust reads the question Cursor's hooks cannot raise: Codex refuses to run a hook it
// has not been shown, so a committed file is inert on a fresh clone until the developer
// trusts it once, from inside Codex. The wiring looks perfect and nothing runs, which
// is a silence worth naming.
func (c Agent) Trust(root string) (agents.TrustState, error) {
	hooksPath := filepath.Join(root, filepath.FromSlash(c.HooksPath()))
	trust, err := (Codex{}).CodexHookTrustFor(hooksPath)
	if err != nil {
		return agents.TrustState{}, err
	}
	switch {
	case !trust.Reviewed():
		return agents.TrustState{
			Detail: ", but Codex has not been shown them yet, so it runs none of them",
			Fix:    codexHookReview,
		}, nil
	case trust.Trusted == 0:
		return agents.TrustState{
			Detail: ", but none are trusted, so Codex runs none of them",
			Fix:    codexHookReview,
		}, nil
	case trust.Disabled > 0:
		return agents.TrustState{
			Detail: fmt.Sprintf(", but %d is switched off in Codex", trust.Disabled),
			Fix:    "open this repository in Codex Desktop and re-enable Terma's hooks in Settings → Hooks (or use /hooks in Codex CLI)",
		}, nil
	}
	// Codex trusts a hook entry by entry. A file that was trusted before terma added an
	// entry to it — the two subagent hooks arrived that way — still reads as trusted by
	// the count, while Codex skips the new ones and says nothing.
	entries, err := TermaEntries(root)
	if err != nil {
		return agents.TrustState{}, err
	}
	var skipped []string
	for _, e := range entries {
		if trust.TrustedHashes[e.Key()] != e.Hash {
			skipped = append(skipped, e.Event)
		}
	}
	if len(skipped) > 0 {
		return agents.TrustState{
			Detail: fmt.Sprintf(", but Codex needs to review %s, so it skips %s", strings.Join(skipped, ", "), pronoun(len(skipped))),
			Fix:    codexHookReview + "; include any new or changed entries",
		}, nil
	}
	return agents.TrustState{Trusted: true, Detail: " and trusted"}, nil
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// Harness is how terma configures the agent's exporter.
func (Agent) Harness() harness.Harness { return Codex{} }

// WhenHooksOff runs the developer's own notifier: terma replaced Codex's direct notify
// invocation, so it must still fire with capture off.
func (Agent) WhenHooksOff() map[string]agents.Handler {
	return map[string]agents.Handler{"codex-notify": func(ctx context.Context, env hookrun.Env) error {
		if len(env.Args) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return RunPreviousCodexNotify(ctx, env.Args[0])
	}}
}

// ContentConsented is the consent replies and thread names travel under.
func (Agent) ContentConsented(projectID string, global bool) bool {
	return CodexRepliesConsented(projectID, global)
}

// RetrustNote is what a refresh that rewrote .codex/hooks.json says.
func (Agent) RetrustNote() string {
	return "Codex runs changed hooks only after you trust them again in Codex; `terma doctor` names any it is skipping."
}

// Coverage is how completely terma supports the agent.
func (Agent) Coverage() (attribution, telemetry agents.CapabilitySupport) {
	return agents.CapabilitySupport{Level: agents.SupportFull},
		agents.CapabilitySupport{Level: agents.SupportFull, Note: "export is machine-wide; it cannot be scoped to a repository"}
}

// UserHooksTrustStep is how the developer trusts the machine-wide hooks.
func (Agent) UserHooksTrustStep() string {
	return "Codex runs its machine-wide hooks once you trust them: in Codex, open `/hooks` (Desktop: Settings → Hooks → Review) and approve Terma's. An organization that deploys them as managed configuration skips this step."
}

// NotifierInstalled reports whether terma's notifier is in Codex's config.
func (Agent) NotifierInstalled() (bool, error) { return Codex{}.NotifierInstalled() }

// InstallNotifier chains terma's notifier in front of the developer's.
func (Agent) InstallNotifier() (bool, error) { return Codex{}.InstallNotifier() }

// RemoveNotifier puts back the developer's notifier.
func (Agent) RemoveNotifier() (bool, error) { return Codex{}.RemoveNotifier() }

var (
	_ agents.Notifier       = Agent{}
	_ agents.UserHooksTrust = Agent{}
	_ agents.Covered        = Agent{}
	_ agents.SurfaceChecker = Agent{}
	_ agents.Retrusting     = Agent{}
	_ agents.ContentConsent = Agent{}
	_ agents.OffSwitched    = Agent{}
	_ agents.Exporting      = Agent{}
	_ agents.Agent          = Agent{}
	_ agents.Trusting       = Agent{}
	_ agents.UserHooks      = Agent{}
	_ agents.ManagedHooks   = Agent{}
	_ agents.Surfaced       = Agent{}
)

// userHooksPath is the user hook file shared by Codex's CLI and Desktop.
func userHooksPath() (string, error) {
	path, err := (Codex{}).ConfigPath()
	return filepath.Join(filepath.Dir(path), "hooks.json"), err
}
