package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

type routedFake struct {
	fn agents.Handler
}

func (routedFake) Name() string                   { return "rfake" }
func (routedFake) DisplayName() string            { return "RFake" }
func (routedFake) Installed(context.Context) bool { return true }
func (routedFake) FlushAfter() []string           { return nil }
func (r routedFake) Events() map[string]agents.Handler {
	return map[string]agents.Handler{"rfake-hook": r.fn}
}

func TestDispatchUsesClaimedRoutedWhenRoutingActive(t *testing.T) {
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	rt := hookrun.NewRouting("default", nil)
	var legacyCalled, routedCalled bool
	var gotRT *hookrun.Routing

	deps := Deps{
		StateDir: stateDir,
		Agents: agents.New(routedFake{fn: func(_ context.Context, env hookrun.Env) error {
			if env.Routing != rt {
				t.Errorf("env.Routing = %v; want %v", env.Routing, rt)
			}
			if env.OnClaim != nil {
				env.OnClaim("/tmp/repo")
			}
			return nil
		}}),
		Profile:       func() Profile { return Profile{Policy: config.NoPolicy("", ""), Routing: rt} },
		Spool:         func() *spool.Spool { return nil },
		Claimed:       func(string, config.Policy) { legacyCalled = true },
		ClaimedRouted: func(_ string, got *hookrun.Routing) { routedCalled, gotRT = true, got },
	}
	status := Run(context.Background(), deps, Request{Event: "rfake-hook", Cwd: t.TempDir(), Stdin: strings.NewReader("{}")})
	if status != 0 {
		t.Fatalf("status = %d; want 0", status)
	}
	if legacyCalled || !routedCalled || gotRT != rt {
		t.Fatalf("legacy=%v routed=%v gotRT=%v; want false, true, %v", legacyCalled, routedCalled, gotRT, rt)
	}
}
