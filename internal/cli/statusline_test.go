package cli

import (
	"bytes"
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
