package adapter

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// gemini is Gemini CLI. Its hooks come from terma's user-level Gemini extension
// (harness.ConnectGeminiRelay), which fires in every folder; a repository's own
// .gemini/settings.json hooks need the folder trusted. It is an adapter so `terma hook
// gemini-*` dispatches from the same table as everyone else's.
type gemini struct{}

func (gemini) Name() string        { return "gemini" }
func (gemini) DisplayName() string { return "Gemini CLI" }
func (gemini) Installed(context.Context) bool {
	_, err := exec.LookPath("gemini")
	return err == nil
}
func (gemini) HooksPath() string   { return "" }
func (gemini) Default(string) bool { return false }
func (gemini) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (gemini) Events() map[string]Handler {
	return map[string]Handler{
		"gemini-session-start": hookrun.GeminiSessionStart,
		"gemini-prompt":        hookrun.GeminiPrompt,
		"gemini-after-tool":    hookrun.GeminiAfterTool,
		"gemini-session-end":   hookrun.GeminiSessionEnd,
	}
}

func (gemini) FlushAfter() []string { return []string{"gemini-session-end"} }
