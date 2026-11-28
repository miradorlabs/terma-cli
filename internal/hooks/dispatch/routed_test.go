package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
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
	var routedCalled bool
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
		Profile:       func() Profile { return Profile{Routing: rt} },
		Spool:         func() *spool.Spool { return nil },
		ClaimedRouted: func(_ string, got *hookrun.Routing) { routedCalled, gotRT = true, got },
	}
	status := Run(context.Background(), deps, Request{Event: "rfake-hook", Cwd: t.TempDir(), Stdin: strings.NewReader("{}")})
	if status != 0 {
		t.Fatalf("status = %d; want 0", status)
	}
	if !routedCalled || gotRT != rt {
		t.Fatalf("routed=%v gotRT=%v; want true, %v", routedCalled, gotRT, rt)
	}
}

type extFake struct{}

func (extFake) Name() string                   { return "extfake" }
func (extFake) DisplayName() string            { return "ExtFake" }
func (extFake) Installed(context.Context) bool { return true }
func (extFake) FlushAfter() []string           { return nil }
func (extFake) Events() map[string]agents.Handler {
	return hookrun.Extension{Tool: "omp"}.Events("extfake")
}

// A plugin installed by an earlier build still sends <tool>-tool-reach until setup refreshes
// it: the hook takes it, writes nothing and exits 0, routed or not.
func TestDispatchLegacyToolReachIsANoOp(t *testing.T) {
	stateDir := t.TempDir()
	hookruntest.RelayOn(t, stateDir)
	repo := t.TempDir()
	body := `{"session_id":"018f3a2c-9109-7000-8000-000000000001","cwd":` + strconv.Quote(repo) + `,"tool":"read","paths":["/elsewhere/secret.go"]}`
	for _, rt := range []*hookrun.Routing{nil, hookrun.NewRouting("default", nil)} {
		var claimed bool
		deps := Deps{
			StateDir:      stateDir,
			Agents:        agents.New(extFake{}),
			Profile:       func() Profile { return Profile{Routing: rt} },
			Spool:         func() *spool.Spool { return nil },
			ClaimedRouted: func(string, *hookrun.Routing) { claimed = true },
		}
		if code := Run(context.Background(), deps, Request{Event: "extfake-tool-reach", Cwd: repo, Stdin: strings.NewReader(body)}); code != 0 {
			t.Fatalf("extfake-tool-reach (routing %v) exited %d; want 0", rt != nil, code)
		}
		if claimed {
			t.Fatalf("extfake-tool-reach (routing %v) claimed the session", rt != nil)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, "runs"))
	if len(entries) != 0 {
		t.Fatalf("extfake-tool-reach wrote run state: %v", entries)
	}
}
