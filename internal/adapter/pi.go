package adapter

import (
	"context"
	"os/exec"

	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// pi is Pi (@earendil-works/pi-coding-agent). Like OpenCode it has no repository-scope
// hooks: terma's user-scope extension (internal/harness/pi/terma.ts) calls the binary
// with the events below. It is an adapter so `terma hook pi-*` dispatches from the same
// table as everyone else's.
type pi struct{}

func (pi) Name() string        { return "pi" }
func (pi) DisplayName() string { return "Pi" }
func (pi) Installed(context.Context) bool {
	_, err := exec.LookPath("pi")
	return err == nil
}
func (pi) HooksPath() string   { return "" }
func (pi) Default(string) bool { return false }
func (pi) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (pi) Events() map[string]Handler {
	return map[string]Handler{
		"pi-session-start": hookrun.PiSessionStart,
		"pi-prompt":        hookrun.PiPrompt,
		"pi-session-end":   hookrun.PiSessionEnd,
		"pi-file-edit":     hookrun.PiFileEdit,
	}
}

func (pi) FlushAfter() []string { return []string{"pi-session-end"} }
