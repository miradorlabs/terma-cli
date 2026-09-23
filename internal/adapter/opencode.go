package adapter

import (
	"context"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
)

// opencode is OpenCode. It has no repository-scope hooks: Terma's plugin is user-scope,
// written by `terma install` / `terma connect opencode`, and calls the binary directly with
// the events below. It is an adapter here so `--adapters opencode` is a known name (a
// repository can record it and carry an export policy for it) and so its events are
// dispatched from the same table as everyone else's.
type opencode struct{}

func (opencode) Name() string                       { return "opencode" }
func (opencode) DisplayName() string                { return "OpenCode" }
func (opencode) Installed(ctx context.Context) bool { return harness.OpenCode{}.Detect(ctx).Found }
func (opencode) HooksPath() string                  { return "" }
func (opencode) Default(string) bool                { return false }
func (opencode) Plan(string, bool) (hookmgr.Plan, error) {
	return hookmgr.Plan{}, nil
}

func (opencode) Events() map[string]Handler {
	return map[string]Handler{
		"opencode-session-start": hookrun.OpenCodeSessionStart,
		"opencode-session-end":   hookrun.OpenCodeSessionEnd,
		"opencode-file-edit":     hookrun.OpenCodeFileEdit,
	}
}

func (opencode) FlushAfter() []string { return []string{"opencode-session-end"} }
