package adapter

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// codex is OpenAI's Codex CLI. Its repository half is .codex/hooks.json, wired by
// default where the repository carries a .codex directory; its user half (`notify` in
// ~/.codex/config.toml, written by `terma connect codex`) reaches the same handler set through
// the codex-notify event.
type codex struct{}

func (codex) Name() string                       { return "codex" }
func (codex) DisplayName() string                { return "Codex" }
func (codex) Installed(ctx context.Context) bool { return harness.Codex{}.Detect(ctx).Found }
func (codex) HooksPath() string                  { return hookmgr.CodexHooksPath }
func (codex) Default(root string) bool           { return hookmgr.HasCodex(root) }
func (codex) Plan(root string, install bool) (hookmgr.Plan, error) {
	return hookmgr.PlanCodexHooks(root, install)
}

func (codex) Events() map[string]Handler {
	return map[string]Handler{
		"codex-notify":         hookrun.CodexNotify,
		"codex-session-start":  hookrun.CodexSessionStart,
		"codex-session-end":    hookrun.CodexSessionEnd,
		"codex-post-tool-use":  hookrun.CodexPostToolUse,
		"codex-stop":           hookrun.CodexStop,
		"codex-subagent-start": hookrun.CodexSubagentStart,
		"codex-subagent-stop":  hookrun.CodexSubagentStop,
	}
}

func (codex) FlushAfter() []string {
	return []string{"codex-notify", "codex-session-end", "codex-stop"}
}

// Trust reads the question Cursor's hooks cannot raise: Codex refuses to run a hook it
// has not been shown, so a committed file is inert on a fresh clone until the developer
// trusts it once, from inside Codex. The wiring looks perfect and nothing runs, which
// is a silence worth naming.
func (c codex) Trust(root string) (TrustState, error) {
	hooksPath := filepath.Join(root, filepath.FromSlash(c.HooksPath()))
	trust, err := (harness.Codex{}).CodexHookTrustFor(hooksPath)
	if err != nil {
		return TrustState{}, err
	}
	switch {
	case !trust.Reviewed():
		return TrustState{
			Detail: ", but Codex has not been shown them yet, so it runs none of them",
			Fix:    "open Codex in this repository and run /hooks to review and trust them",
		}, nil
	case trust.Trusted == 0:
		return TrustState{
			Detail: ", but none are trusted, so Codex runs none of them",
			Fix:    "open Codex in this repository and run /hooks to trust them",
		}, nil
	case trust.Disabled > 0:
		return TrustState{
			Detail: fmt.Sprintf(", but %d is switched off in Codex", trust.Disabled),
			Fix:    "open Codex in this repository and run /hooks to re-enable them",
		}, nil
	}
	// Codex trusts a hook entry by entry. A file that was trusted before terma added an
	// entry to it — the two subagent hooks arrived that way — still reads as trusted by
	// the count, while Codex skips the new ones and says nothing.
	entries, err := hookmgr.CodexTermaEntries(root)
	if err != nil {
		return TrustState{}, err
	}
	var skipped []string
	for _, e := range entries {
		if !trust.TrustedKeys[e.Key()] {
			skipped = append(skipped, e.Event)
		}
	}
	if len(skipped) > 0 {
		return TrustState{
			Detail: fmt.Sprintf(", but Codex has not trusted %s yet, so it skips %s", strings.Join(skipped, ", "), pronoun(len(skipped))),
			Fix:    "open Codex in this repository and run /hooks to trust the new entries",
		}, nil
	}
	return TrustState{Trusted: true, Detail: " and trusted"}, nil
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
