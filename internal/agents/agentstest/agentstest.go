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

// Default is false: install never wires it unasked.

// Plan plans nothing.

// Events handles no event.
func (Agent) Events() map[string]agents.Handler { return nil }

// FlushAfter flushes after no event.
func (Agent) FlushAfter() []string { return nil }

// Tool is the agent's label.
func (a Agent) Tool() string { return cmp.Or(a.Label, a.ID) }

// UserHooked is an Agent with machine-wide hooks: a JSON hooks file, with one Stop hook
// running the command it is given for "<id>-stop".
type UserHooked struct {
	Agent
	// Path is the agent's hooks file.
	Path string
	// Others are more hooks files beside Path, for an agent with several homes on one
	// machine: its own, and one for each copy another app runs with a folder of its own.
	// Each home has a hooks file, and setup, teardown and doctor treat every file alike
	// (agents.UserHooksFiles).
	Others []string
	// LeftOut is the note a setup returns instead of writing anything: when it is set, an
	// install changes no file and reports the note. A real agent does this for a home whose
	// installed version is too old to run the hooks.
	LeftOut string
}

// UserHooksPath is Path.
func (u UserHooked) UserHooksPath() (string, error) { return u.Path, nil }

// UserHooksPaths is Path, then Others.
func (u UserHooked) UserHooksPaths() ([]string, error) {
	return append([]string{u.Path}, u.Others...), nil
}

// PlanUserHooks merges the Stop hook into the file in dir, or plans LeftOut's note.
func (u UserHooked) PlanUserHooks(dir string, command func(string) string, install bool) (hookmgr.Plan, error) {
	if install && u.LeftOut != "" {
		return hookmgr.Plan{Notes: []string{u.LeftOut}}, nil
	}
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
	_ agents.UserHooksFiles   = UserHooked{}
)
