package adapter

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// dsh is DeepSeek Harness. terma's user-level Cordis plugin (internal/harness/dsh)
// calls the binary with the events below; it is an adapter so `terma hook dsh-*`
// dispatches from the same table as everyone else's.
type dsh struct{}

func (dsh) Name() string        { return "dsh" }
func (dsh) DisplayName() string { return "DeepSeek Harness" }
func (dsh) Installed(context.Context) bool {
	_, err := exec.LookPath("dsh")
	return err == nil
}
func (dsh) HooksPath() string   { return "" }
func (dsh) Default(string) bool { return false }
func (dsh) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (dsh) Events() map[string]Handler {
	return map[string]Handler{
		"dsh-session-start": hookrun.DshSessionStart,
		"dsh-prompt":        hookrun.DshPrompt,
		"dsh-session-end":   hookrun.DshSessionEnd,
		"dsh-file-edit":     hookrun.DshFileEdit,
	}
}

func (dsh) FlushAfter() []string { return []string{"dsh-session-end"} }
