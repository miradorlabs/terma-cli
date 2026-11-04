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

// desktopTraced is a tracedAgent whose own command is missing, run from a desktop app instead.
type desktopTraced struct {
	tracedAgent
	desktop bool
}

func (desktopTraced) Installed(context.Context) bool { return false }

func (a desktopTraced) Surfaces() []agents.Surface {
	return []agents.Surface{{Name: a.Name(), Installed: func(context.Context) bool { return a.desktop }}}
}

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
	desktop := func(installed bool) *agents.Registry {
		return agents.New(desktopTraced{tracedAgent{envAgent{agentstest.Agent{ID: "fake"}}, []string{"BETA=1"}, nil}, installed})
	}
	if c, ok := TracesCheck(ctx, desktop(true), []string{"fake"}, t.TempDir(), nil, time.Now()); !ok || c.Status != Warn {
		t.Fatalf("desktop only = %+v, %v; want a warning", c, ok)
	}
	if _, ok := TracesCheck(ctx, desktop(false), nil, t.TempDir(), nil, time.Now()); ok {
		t.Fatal("an agent with no surface installed was checked")
	}
}

// Another agent's process terma cannot read says nothing about this one's.
func TestTracesCheckNamesOnlyItsOwnUnreadProcess(t *testing.T) {
	t.Parallel()
	reg := agents.New(tracedAgent{envAgent: envAgent{agentstest.Agent{ID: "fake"}}}, envAgent{agentstest.Agent{ID: "other"}})
	dir := t.TempDir()
	for id, c := range map[string]claim.Claim{"s1": {ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: []int{201}}, "s2": {ProjectID: "p1", Tool: "other", Cwd: "/work/a", PIDs: []int{202}}} {
		claim.Write(dir, id, c, time.Now())
	}
	c, _ := TracesCheck(context.Background(), reg, nil, dir, processes(map[int][]string{201: {"HOME=/home/dev"}, 202: nil}), time.Now())
	if c.Status != Pass || strings.Contains(c.Detail, "cannot read") {
		t.Fatalf("check = %+v, want a pass that does not say fake's process is unread", c)
	}
}

// Processes sharing what switches spans off are named together.
func TestTracesCheckGroupsProcesses(t *testing.T) {
	t.Parallel()
	reg := agents.New(tracedAgent{envAgent: envAgent{agentstest.Agent{ID: "fake"}}})
	dir := t.TempDir()
	for id, pid := range map[string]int{"s1": 201, "s2": 202} {
		claim.Write(dir, id, claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: []int{pid}}, time.Now())
	}
	table := map[int][]string{201: {"SPANS=off"}, 202: {"SPANS=off"}}
	c, _ := TracesCheck(context.Background(), reg, nil, dir, processes(table), time.Now())
	if c.Status != Warn || strings.Count(c.Detail, "started with") != 1 || !strings.Contains(c.Detail, "pids 201, 202") {
		t.Fatalf("check = %+v, want one finding naming pids 201 and 202", c)
	}
}
