// Package codex integrates Codex, the CLI and Desktop.
package codex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// Agent covers OpenAI's Codex CLI and Desktop repository hooks in .codex/hooks.json.
// The CLI's user-level notifier (`notify` in ~/.codex/config.toml, written by
// `terma connect codex`) reaches the same handler set through codex-notify.
type Agent struct{}

const codexHookReview = "open this repository in Codex Desktop, then go to Settings → Hooks → Review and approve the Terma entries (or run /hooks in Codex CLI)"

func (Agent) Name() string                       { return "codex" }
func (Agent) DisplayName() string                { return "Codex" }
func (Agent) Installed(ctx context.Context) bool { return harness.Codex{}.Detect(ctx).Found }
func (Agent) HooksPath() string                  { return hookmgr.CodexHooksPath }
func (Agent) Default(root string) bool           { return hookmgr.HasCodex(root) }
func (Agent) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCodexHooks(root, install)
}

func (Agent) Events() map[string]agents.Handler {
	return map[string]agents.Handler{
		"codex-notify":             hookrun.CodexNotify,
		"codex-session-start":      hookrun.CodexSessionStart,
		"codex-user-prompt-submit": hookrun.CodexUserPromptSubmit,
		"codex-pre-tool-use":       hookrun.CodexPreToolUse,
		"codex-permission-request": hookrun.CodexPermissionRequest,
		"codex-session-end":        hookrun.CodexSessionEnd,
		"codex-post-tool-use":      hookrun.CodexPostToolUse,
		"codex-stop":               hookrun.CodexStop,
		"codex-subagent-start":     hookrun.CodexSubagentStart,
		"codex-subagent-stop":      hookrun.CodexSubagentStop,
	}
}

func (Agent) FlushAfter() []string {
	return []string{"codex-notify", "codex-session-end", "codex-stop", "codex-user-prompt-submit"}
}

func (Agent) UserHooksPath() (string, error) { return harness.CodexUserHooksPath() }
func (Agent) Selections() []string           { return []string{"codex", "codex-desktop"} }
func (Agent) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCodexUserHooks(dir, command, install)
}
func (Agent) ManagedHookFiles(root string) []string { return harness.CodexManagedHookFiles(root) }

// Trust reads the question Cursor's hooks cannot raise: Codex refuses to run a hook it
// has not been shown, so a committed file is inert on a fresh clone until the developer
// trusts it once, from inside Codex. The wiring looks perfect and nothing runs, which
// is a silence worth naming.
func (c Agent) Trust(root string) (agents.TrustState, error) {
	hooksPath := filepath.Join(root, filepath.FromSlash(c.HooksPath()))
	trust, err := (harness.Codex{}).CodexHookTrustFor(hooksPath)
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
	entries, err := hookmgr.CodexTermaEntries(root)
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

var (
	_ agents.Agent        = Agent{}
	_ agents.Trusting     = Agent{}
	_ agents.UserHooks    = Agent{}
	_ agents.ManagedHooks = Agent{}
	_ agents.Selector     = Agent{}
)
