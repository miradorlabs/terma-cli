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

// name keys the keystore and routing records too.
const name = "codex"

// Agent covers Codex's repository hooks in .codex/hooks.json and its user-level notifier.
type Agent struct{ exporter }

const codexHookReview = "open this repository in Codex Desktop, then go to Settings → Hooks → Review and approve the Terma entries (or run /hooks in Codex CLI)"

func (Agent) Installed(ctx context.Context) bool { return exporter{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hooksPath }
func (Agent) Default(root string) bool           { return hasConfig(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return planHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"codex-notify":             notifyHook,
		"codex-session-start":      sessionStart,
		"codex-user-prompt-submit": userPromptSubmit,
		"codex-pre-tool-use":       preToolUse,
		"codex-permission-request": permissionRequest,
		"codex-session-end":        sessionEnd,
		"codex-post-tool-use":      postToolUse,
		"codex-stop":               stop,
		"codex-subagent-start":     subagentStart,
		"codex-subagent-stop":      subagentStop,
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

// ManagedConfig is a requirements.toml of terma's hooks, which Codex runs with no trust step.
func (Agent) ManagedConfig(command func(string) string) (string, []byte, error) {
	return "codex-requirements.toml", []byte(managedRequirements(command)), nil
}

// ManagedDeploy says where requirements.toml goes.
func (Agent) ManagedDeploy() string {
	return "`/etc/codex/requirements.toml` (append to one you already deploy), or\n  the same table in your MDM profile for `com.openai.codex`."
}

// Trust reads Codex's trust records: a committed hooks file is inert until the developer
// trusts it from inside Codex, and nothing says so.
func (c Agent) Trust(root string) (agents.TrustState, error) {
	hooksPath := filepath.Join(root, filepath.FromSlash(c.HooksPath()))
	trust, err := (exporter{}).hookTrustFor(hooksPath)
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
	// A file trusted before terma added an entry still counts as trusted while Codex skips the
	// new entry in silence.
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
func (Agent) Harness() harness.Harness { return exporter{} }

// WhenHooksOff runs the developer's own notifier: terma replaced Codex's direct notify
// invocation, so it must still fire with capture off.
func (Agent) WhenHooksOff() map[string]agents.Handler {
	return map[string]agents.Handler{"codex-notify": func(ctx context.Context, env hookrun.Env) error {
		if len(env.Args) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return runPreviousNotify(ctx, env.Args[0])
	}}
}

// ContentConsented is the consent replies and thread names travel under.
func (Agent) ContentConsented(projectID string, global bool) bool {
	return repliesConsented(projectID, global)
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
	path, err := (exporter{}).ConfigPath()
	return filepath.Join(filepath.Dir(path), "hooks.json"), err
}
