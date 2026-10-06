package doctor

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
)

// gatedAgent runs its machine-wide hooks only once trusted.
type gatedAgent struct {
	agentstest.Agent
	present, trusted bool
}

func (gatedAgent) UserHooksTrustStep() string { return "approve them" }
func (g gatedAgent) UserHooksTrusted() (bool, bool, error) {
	return g.present, g.trusted, nil
}

// Machine-wide hooks an agent skips warn, as a repository's untrusted ones do; an agent
// with none, or one the developer does not use, adds no line at all.
func TestUserHooksCheckWarnsWhenAnAgentSkipsThem(t *testing.T) {
	t.Parallel()
	reg := agents.New(gatedAgent{Agent: agentstest.Agent{ID: "gated"}, present: true})
	c, ok := UserHooksCheck(reg, nil)
	if !ok || c.Status != Warn || c.Fix != "approve them" {
		t.Fatalf("untrusted = %+v, %v", c, ok)
	}
	if _, ok := UserHooksCheck(reg, []string{"other"}); ok {
		t.Fatal("an agent the developer does not use was checked")
	}
	reg = agents.New(gatedAgent{Agent: agentstest.Agent{ID: "gated"}, present: true, trusted: true})
	if c, ok := UserHooksCheck(reg, nil); !ok || c.Status != Pass {
		t.Fatalf("trusted = %+v, %v", c, ok)
	}
	reg = agents.New(gatedAgent{Agent: agentstest.Agent{ID: "gated"}})
	if _, ok := UserHooksCheck(reg, nil); ok {
		t.Fatal("an agent with no machine-wide hooks was checked")
	}
}
