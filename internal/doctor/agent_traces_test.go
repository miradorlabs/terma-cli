package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// tracedAgent is an agent whose spans need switches of their own: settingsOff is what its
// settings lack, or settingsErr why they cannot be read; SPANS=off in a running process's
// environment switches them off.
type tracedAgent struct {
	envAgent
	settingsOff []string
	settingsErr error
}

func (a tracedAgent) TracesOff() ([]string, error) { return a.settingsOff, a.settingsErr }

func (tracedAgent) TracesOffIn(env []string) []string {
	for _, kv := range env {
		if kv == "SPANS=off" {
			return []string{"SPANS"}
		}
	}
	return nil
}

func (tracedAgent) SpansCarry() string { return "finish reason" }

// The spans check warns, naming what is lost and where it is switched off, from the settings
// and from running processes' environments; it passes when both export spans, and never
// passes on settings it could not read.
func TestTracesCheckNamesWhatSpansCarry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	agent := func(err error, off ...string) *agents.Registry {
		return agents.New(tracedAgent{envAgent{agentstest.Agent{ID: "fake"}}, off, err})
	}
	clean := []string{"HOME=/home/dev"}
	for _, tc := range []struct {
		name   string
		reg    *agents.Registry
		table  map[int][]string
		status Status
		want   []string
	}{
		{"exported", agent(nil), map[int][]string{agentPID: clean}, Pass, []string{"fake exports its spans"}},
		{"a running process unread", agent(nil), map[int][]string{agentPID: nil}, Pass, []string{"by its settings; terma cannot read"}},
		{"settings lack the switches", agent(nil, "BETA=1"), nil, Warn,
			[]string{"fake exports no spans (its settings lack BETA=1)", "finish reason", "terma setup"}},
		{"settings unreadable", agent(errors.New("parse settings.json")), nil, Warn,
			[]string{"terma cannot read its settings: parse settings.json", "repair the settings file"}},
		{"a running process switches them off", agent(nil), map[int][]string{agentPID: {"SPANS=off"}}, Warn,
			[]string{"SPANS in the environment pid 200 started with switches them off", "remove that variable", "restart the agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ok := TracesCheck(ctx, tc.reg, nil, claimed(t, "fake"), processes(tc.table), time.Now())
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
	if _, ok := TracesCheck(ctx, agents.New(agentstest.Agent{ID: "plain"}), nil, t.TempDir(), nil, time.Now()); ok {
		t.Fatal("an agent without span switches was checked")
	}
	if _, ok := TracesCheck(ctx, agent(nil, "BETA=1"), []string{"other"}, t.TempDir(), nil, time.Now()); ok {
		t.Fatal("an agent the developer does not use was checked")
	}
}

// Processes sharing what switches spans off are named together.
func TestTracesCheckGroupsProcesses(t *testing.T) {
	t.Parallel()
	reg := agents.New(tracedAgent{envAgent: envAgent{agentstest.Agent{ID: "fake"}}})
	dir := t.TempDir()
	for id, pid := range map[string]int{"s1": 201, "s2": 202} {
		claim.Write(dir, id, claim.Claim{ProjectID: "p1", Tool: "fake", PIDs: []int{pid}}, time.Now())
	}
	table := map[int][]string{201: {"SPANS=off"}, 202: {"SPANS=off"}}
	c, _ := TracesCheck(context.Background(), reg, nil, dir, processes(table), time.Now())
	if c.Status != Warn || strings.Count(c.Detail, "started with") != 1 || !strings.Contains(c.Detail, "pids 201, 202") {
		t.Fatalf("check = %+v, want one finding naming pids 201 and 202", c)
	}
}
