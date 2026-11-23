package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
)

// liner is a status-line agent that records what setup asked of it.
type liner struct {
	agentstest.Agent
	installed, removed *bool
}

func (l liner) InstallStatusLine() (bool, error) { *l.installed = true; return true, nil }
func (l liner) StatusLineState(string) (agents.StatusLineState, error) {
	return agents.StatusLineState{Installed: *l.installed}, nil
}
func (l liner) RemoveStatusLine() (bool, error) { *l.removed = true; return true, nil }

// switchedOff is a status-line agent whose terma line is there but switched off until setup.
type switchedOff struct {
	agentstest.Agent
	on *bool
}

func (s switchedOff) InstallStatusLine() (bool, error) { *s.on = true; return true, nil }
func (s switchedOff) StatusLineState(string) (agents.StatusLineState, error) {
	return agents.StatusLineState{ConfigPath: "/home/dev/.agent/settings.json", Installed: *s.on, Disabled: !*s.on, Stacked: true}, nil
}
func (s switchedOff) RemoveStatusLine() (bool, error) { return false, nil }

// Setup says when it switches terma's status line back on.
func TestSetupStatusLineSaysWhenItSwitchesItBackOn(t *testing.T) {
	on := false
	app := &App{agents: agents.New(switchedOff{agentstest.Agent{ID: "one"}, &on})}
	var out bytes.Buffer
	ui := newSetupUI(&out, false)
	app.setupStatusLine(&out, ui, []string{"one"})
	ui.printLines()
	if !on || !strings.Contains(out.String(), "switched back on") {
		t.Fatalf("on=%v output:\n%s", on, out.String())
	}
}

// Setup wraps the status line of each agent chosen and puts back the one of an agent no
// longer chosen, as it takes that agent's hooks out.
func TestSetupStatusLineFollowsTheSelection(t *testing.T) {
	var chosenIn, chosenOut, droppedIn, droppedOut bool
	chosen := liner{agentstest.Agent{ID: "one"}, &chosenIn, &chosenOut}
	dropped := liner{agentstest.Agent{ID: "two"}, &droppedIn, &droppedOut}
	app := &App{agents: agents.New(chosen, dropped)}
	var out bytes.Buffer
	app.setupStatusLine(&out, newSetupUI(&out, false), []string{"one"})
	if !chosenIn || chosenOut || droppedIn || !droppedOut {
		t.Fatalf("chosen installed=%v removed=%v; dropped installed=%v removed=%v\n%s", chosenIn, chosenOut, droppedIn, droppedOut, out.String())
	}
}
