package adapter

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// hermes is Hermes (Nous Research). Its shell hooks are user-level only and do not
// fire in its TUI, so terma's user-level plugin (internal/harness/hermes) calls the
// binary with the events below. It is an adapter so `terma hook hermes-*` dispatches
// from the same table as everyone else's.
type hermes struct{}

func (hermes) Name() string        { return "hermes" }
func (hermes) DisplayName() string { return "Hermes" }
func (hermes) Installed(context.Context) bool {
	_, err := exec.LookPath("hermes")
	return err == nil
}
func (hermes) HooksPath() string   { return "" }
func (hermes) Default(string) bool { return false }
func (hermes) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (hermes) Events() map[string]Handler {
	return map[string]Handler{
		"hermes-session-start": hookrun.HermesSessionStart,
		"hermes-prompt":        hookrun.HermesPrompt,
		"hermes-session-end":   hookrun.HermesSessionEnd,
		"hermes-file-edit":     hookrun.HermesFileEdit,
	}
}

func (hermes) FlushAfter() []string { return []string{"hermes-session-end"} }
