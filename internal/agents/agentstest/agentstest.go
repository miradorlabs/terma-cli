// Package agentstest is a made-up agent for the tests of packages that iterate the
// registry, so they never depend on a real agent's shape.
package agentstest

import (
	"cmp"
	"context"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// Agent has only what every agent has, named ID and labelled Label (ID when empty).
type Agent struct {
	ID, Label string
}

// Name is the agent's ID.
func (a Agent) Name() string { return a.ID }

// DisplayName is the agent's ID too.
func (a Agent) DisplayName() string { return a.ID }

// Installed is always true.
func (Agent) Installed(context.Context) bool { return true }

// HooksPath is "": the agent commits no hooks file.
func (Agent) HooksPath() string { return "" }

// Default is false: install never wires it unasked.
func (Agent) Default(string) bool { return false }

// Plan plans nothing.
func (Agent) Plan(string, bool) (hookmgr.Plan, error) { return hookmgr.Plan{}, nil }

// Events handles no event.
func (Agent) Events() map[string]agents.Handler { return nil }

// FlushAfter flushes after no event.
func (Agent) FlushAfter() []string { return nil }

// Tool is the agent's label.
func (a Agent) Tool() string { return cmp.Or(a.Label, a.ID) }

// UserHooked is an Agent with machine-wide hooks: a JSON file at Path with one Stop hook
// running the command it is given for "<id>-stop".
type UserHooked struct {
	Agent
	Path string
}

// UserHooksPath is Path.
func (u UserHooked) UserHooksPath() (string, error) { return u.Path, nil }

// PlanUserHooks merges the Stop hook into the file in dir.
func (u UserHooked) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	g, err := hookmgr.Group("Stop", "", map[string]string{"type": "command", "command": command(u.ID + "-stop")})
	if err != nil {
		return hookmgr.Plan{}, err
	}
	return hookmgr.MergeEventHooks(dir, hookmgr.HooksFile{Path: filepath.Base(u.Path)}, []hookmgr.EventHook{g}, install)
}

// Refreshing is an Agent with a home-directory file a refresh rewrites: Path when it
// changed, Err when it could not.
type Refreshing struct {
	Agent
	Path string
	Err  error
}

// RefreshMachine reports Path, or Err.
func (r Refreshing) RefreshMachine() (string, bool, error) {
	return r.Path, r.Path != "" && r.Err == nil, r.Err
}

var (
	_ agents.MachineRefresher = Refreshing{}
	_ agents.Agent            = Agent{}
	_ agents.Labeled          = Agent{}
	_ agents.UserHooks        = UserHooked{}
)
