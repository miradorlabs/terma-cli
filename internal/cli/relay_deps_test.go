package cli

import (
	"errors"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
)

// trustGated is an agent that runs its machine-wide hooks only once they are trusted.
type trustGated struct {
	agentstest.Agent
	present, trusted bool
	err              error
}

func (trustGated) UserHooksTrustStep() string { return "trust them" }

func (g trustGated) UserHooksTrusted() (bool, bool, error) { return g.present, g.trusted, g.err }

// The relay promises an agent's file edits only where terma's hooks run as written: always for an agent whose claims
// only its hooks write, and for one that gates them on trust only once they are present and trusted.
func TestFileEditHooksFollowsHookTrust(t *testing.T) {
	app := &App{agents: agents.New(
		agentstest.Agent{ID: "hooked"},
		trustGated{Agent: agentstest.Agent{ID: "trusted"}, present: true, trusted: true},
		trustGated{Agent: agentstest.Agent{ID: "untrusted"}, present: true},
		trustGated{Agent: agentstest.Agent{ID: "absent"}},
		trustGated{Agent: agentstest.Agent{ID: "unreadable"}, present: true, trusted: true, err: errors.New("no config")},
	)}
	for tool, want := range map[string]bool{"hooked": true, "trusted": true, "untrusted": false, "absent": false, "unreadable": false, "unknown": false} {
		if got := app.fileEditHooks(tool); got != want {
			t.Errorf("fileEditHooks(%q) = %v, want %v", tool, got, want)
		}
	}
}
