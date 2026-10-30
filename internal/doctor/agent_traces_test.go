package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
)

// tracedAgent is an agent whose spans need switches of their own: settingsOff is what its
// settings lack; a process environment naming SPANS=off switches them off.
type tracedAgent struct {
	envAgent
	settingsOff []string
}

func (a tracedAgent) TracesOff(_ string, env []string) []string {
	if env == nil {
		return a.settingsOff
	}
	for _, kv := range env {
		if kv == "SPANS=off" {
			return []string{"SPANS"}
		}
	}
	return nil
}

func (tracedAgent) SpansCarry() string { return "time to first token" }

// The spans check warns, naming what is lost and where it is switched off, from the settings
// and from a running process's environment; it passes when both export spans.
func TestTracesCheckNamesWhatSpansCarry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agent := func(off ...string) *agents.Registry {
		return agents.New(tracedAgent{envAgent{agentstest.Agent{ID: "fake"}}, off})
	}
	environ := processes(map[int][]string{agentPID: {"SPANS=off"}})
	for _, tc := range []struct {
		name    string
		reg     *agents.Registry
		environ Environ
		status  Status
		want    []string
	}{
		{"exported", agent(), processes(map[int][]string{agentPID: {"HOME=/home/dev"}}), Pass, []string{"fake exports its spans"}},
		{"settings lack the switches", agent("BETA=1"), nil, Warn,
			[]string{"fake exports no spans (its settings lack BETA=1)", "time to first token", "terma setup"}},
		{"a running process switches them off", agent(), environ, Warn,
			[]string{"SPANS in the environment pid 200 started with switches them off", "remove that variable", "restart the agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ok := TracesCheck(ctx, tc.reg, nil, "", claimed(t, "fake"), tc.environ, time.Now())
			if !ok || c.Status != tc.status {
				t.Fatalf("TracesCheck = %+v, %v; want %v", c, ok, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(c.Detail+" "+c.Fix, w) {
					t.Errorf("check %+v lacks %q", c, w)
				}
			}
		})
	}
	if _, ok := TracesCheck(ctx, agents.New(agentstest.Agent{ID: "plain"}), nil, "", t.TempDir(), nil, time.Now()); ok {
		t.Fatal("an agent without span switches was checked")
	}
	if _, ok := TracesCheck(ctx, agent("BETA=1"), []string{"other"}, "", t.TempDir(), nil, time.Now()); ok {
		t.Fatal("an agent the developer does not use was checked")
	}
}
