package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/agentstest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// envAgent is an agent whose exporter reads its startup environment.
type envAgent struct{ agentstest.Agent }

func (envAgent) ExportsFromEnvironment() {}

const relayToken = "0123456789abcdef0123456789abcdef"

// The hook that claimed the session (pid 100) has exited; the agent (pid 200) runs under its host (pid 300).
const (
	hookPID  = 100
	agentPID = 200
	hostPID  = 300
)

// claimed writes a collected session's claim for tool's processes, and returns the state directory.
func claimed(t *testing.T, tool string) string {
	t.Helper()
	dir := t.TempDir()
	if !claim.Write(dir, "s1", claim.Claim{ProjectID: "p1", Tool: tool, Cwd: "/work/a", PIDs: []int{hookPID, agentPID, hostPID}}, time.Now()) {
		t.Fatal("claim not written")
	}
	return dir
}

// processes is a fake process table: each running process's startup environment, nil for
// one terma cannot read; a process not in it has exited.
func processes(table map[int][]string) Environ {
	return func(pid int) ([]string, bool) {
		env, alive := table[pid]
		return env, alive
	}
}

// The check reads the environment the live agent process started with, which outranks the
// settings files, and names each wrong variable without its value.
func TestAgentEnvCheckJudgesTheLiveProcessEnvironment(t *testing.T) {
	t.Parallel()
	reg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	relay := Relay{Addr: "127.0.0.1:43180", Token: relayToken}
	const secret = "ter_old_key_never_printed"
	for _, tc := range []struct {
		name   string
		env    []string
		status Status
		want   []string
		absent []string
	}{
		{name: "no exporter variables: the settings decide", env: []string{"HOME=/home/dev", "PATH=/usr/bin"},
			status: Pass, want: []string{"fake started with no exporter variable", "127.0.0.1:43180"}},
		{name: "the relay with its token", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + relayToken}, status: Pass},
		{name: "the token on the relay's path, no header", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/" + relayToken},
			status: Pass},
		{name: "the token on a per-signal path, a header without it", env: []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:43180/" + relayToken + "/v1/traces",
			"OTEL_EXPORTER_OTLP_HEADERS=x-other=1"}, status: Pass},
		{name: "(a) generic endpoint elsewhere", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"},
			status: Warn, want: []string{"fake (pid 200)", "OTEL_EXPORTER_OTLP_ENDPOINT sends elsewhere than the local relay"},
			absent: []string{"otel.example.com", "signal path"}},
		{name: "(a) per-signal endpoint elsewhere", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://otel.example.com/v1/logs"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT sends elsewhere"},
			absent: []string{"OTEL_EXPORTER_OTLP_ENDPOINT sends", "signal path"}},
		{name: "(a) another port on this machine", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_ENDPOINT sends elsewhere"}},
		{name: "(b) generic endpoint ending in a signal path", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/v1/traces/"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_ENDPOINT ends in a signal path"}, absent: []string{"sends elsewhere"}},
		{name: "(c) headers without the relay's token", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + secret},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_HEADERS lacks the relay's current token"}, absent: []string{"sends elsewhere"}},
		{name: "(c) the token on a foreign endpoint's path", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com/" + relayToken,
			"OTEL_EXPORTER_OTLP_HEADERS=x-other=1"}, status: Warn, want: []string{"sends elsewhere", "OTEL_EXPORTER_OTLP_HEADERS lacks"}},
		{name: "(d) a per-signal endpoint without its route", env: []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:43180"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT names a path the local relay does not serve"}, absent: []string{"sends elsewhere"}},
		{name: "(d) a per-signal endpoint with another signal's route", env: []string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=http://127.0.0.1:43180/v1/traces"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT names a path"}},
		{name: "(d) an old token on the relay's path", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/" + secret},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_ENDPOINT names a path"}},
		{name: "(e) gRPC", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/" + relayToken, "OTEL_EXPORTER_OTLP_PROTOCOL=grpc"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_PROTOCOL picks gRPC"}},
		{name: "(e) gRPC for one signal", env: []string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/grpc"},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL picks gRPC"}, absent: []string{"OTEL_EXPORTER_OTLP_PROTOCOL"}},
		{name: "OTLP/HTTP JSON", env: []string{"OTEL_EXPORTER_OTLP_PROTOCOL=http/json"}, status: Pass},
		{name: "(f) a signal's own authorization without the token", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + relayToken, "OTEL_EXPORTER_OTLP_TRACES_HEADERS=x-other=1, authorization=Bearer " + secret},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_TRACES_HEADERS lacks the relay's current token"}, absent: []string{"OTEL_EXPORTER_OTLP_HEADERS lacks"}},
		{name: "a signal's headers without an authorization", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + relayToken, "OTEL_EXPORTER_OTLP_LOGS_HEADERS=x-other=1"}, status: Pass},
		{name: "the incident: all three", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel-dev.example.com/v1/traces",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + secret},
			status: Warn, want: []string{"OTEL_EXPORTER_OTLP_ENDPOINT sends elsewhere", "ends in a signal path", "OTEL_EXPORTER_OTLP_HEADERS lacks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The host's environment is not the agent's, and is never judged.
			table := map[int][]string{agentPID: tc.env, hostPID: {"OTEL_EXPORTER_OTLP_ENDPOINT=https://host.example.com"}}
			c := AgentEnvCheck(reg, claimed(t, "fake"), relay, processes(table), time.Now())
			if c.Status != tc.status {
				t.Fatalf("status = %v, want %v: %+v", c.Status, tc.status, c)
			}
			for _, w := range tc.want {
				if !strings.Contains(c.Detail, w) {
					t.Errorf("detail %q lacks %q", c.Detail, w)
				}
			}
			for _, a := range append(tc.absent, secret, relayToken, "example.com", "/v1/") {
				if strings.Contains(c.Detail+c.Fix, a) {
					t.Errorf("check %+v says %q", c, a)
				}
			}
			if c.Status == Warn && (!strings.Contains(c.Fix, "IDE or agent host") || !strings.Contains(c.Fix, "shell profile") || !strings.Contains(c.Fix, "restart the agent")) {
				t.Errorf("fix %q does not say where the environment comes from and to restart", c.Fix)
			}
		})
	}
}

// Processes sharing a finding are named together; one that is fine is not named.
func TestAgentEnvCheckGroupsProcessesByFinding(t *testing.T) {
	t.Parallel()
	reg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	dir := t.TempDir()
	now := time.Now()
	for id, pids := range map[string][]int{"s1": {201}, "s2": {202}, "s3": {203}} {
		claim.Write(dir, id, claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: pids}, now)
	}
	bad := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"}
	table := map[int][]string{201: bad, 202: bad, 203: {"HOME=/home/dev"}}
	c := AgentEnvCheck(reg, dir, Relay{Addr: "127.0.0.1:43180", Token: relayToken}, processes(table), now)
	if c.Status != Warn || !strings.Contains(c.Detail, "fake (pids 201, 202)") || strings.Contains(c.Detail, "203") {
		t.Fatalf("check = %+v, want a warning naming pids 201 and 202 together", c)
	}
}

// Whatever chain ends a claim's list, the agent's own is judged: after a run resumed in an
// IDE, after a commit made by hand (whose git hook claimed the session under a shell), with
// the terminal it started in closed, and beside an exited process's id taken by another.
func TestAgentEnvCheckJudgesTheAgentInEveryChain(t *testing.T) {
	t.Parallel()
	reg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	relay := Relay{Addr: "127.0.0.1:43180", Token: relayToken}
	bad := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"}
	clean := []string{"HOME=/home/dev"}
	for _, tc := range []struct {
		name   string
		chains [][]int
		table  map[int][]string
		agent  string
	}{
		// Started from a terminal (hook 101, agent 201, shell 301), resumed in an IDE (hook
		// 102, agent 202, IDE 302); the first agent has exited, its shell has not.
		{"resumed in an IDE", [][]int{{101, 201, 301}, {102, 202, 302}},
			map[int][]string{301: clean, 202: bad, 302: clean}, "fake (pid 202)"},
		// The agent's hook (100), then a commit by hand: hook script 110, git 120, shell 130, terminal 140.
		{"a commit made by hand", [][]int{{hookPID, agentPID, hostPID}, {110, 120, 130, 140}},
			map[int][]string{agentPID: bad, hostPID: clean, 130: clean, 140: clean}, "fake (pid 200)"},
		{"the terminal it started in closed", [][]int{{hookPID, agentPID, hostPID}},
			map[int][]string{agentPID: bad}, "fake (pid 200)"},
		{"an exited process's id taken by another", [][]int{{500, hookPID, agentPID, hostPID}},
			map[int][]string{500: clean, agentPID: bad, hostPID: clean}, "fake (pid 200)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, pids := range tc.chains {
				claim.Write(dir, "s1", claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: pids}, time.Now())
			}
			c := AgentEnvCheck(reg, dir, relay, processes(tc.table), time.Now())
			if c.Status != Warn || !strings.Contains(c.Detail, tc.agent+" started with") || strings.Count(c.Detail, "started with") != 1 {
				t.Fatalf("check = %+v, want a warning about %s alone", c, tc.agent)
			}
		})
	}
}

// With no live process to read, the check skips: none is running, a session is not
// collected, the agent reads no environment, or the platform does not say.
func TestAgentEnvCheckSkipsWithoutALiveProcess(t *testing.T) {
	t.Parallel()
	relay := Relay{Addr: "127.0.0.1:43180", Token: relayToken}
	wrong := map[int][]string{agentPID: {"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"}}
	envReg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	notCollected := t.TempDir()
	claim.Mark(notCollected, "s1", "fake", []int{hookPID, agentPID}, time.Now())
	// Its claim expired, then the developer committed its work from a shell: hook script 400, git 401.
	gitOnly := t.TempDir()
	claim.Write(gitOnly, "s1", claim.Claim{ProjectID: "p1", Tool: "fake", PIDs: []int{400, 401, hostPID}}, time.Now())
	for _, tc := range []struct {
		name     string
		reg      *agents.Registry
		stateDir string
		table    map[int][]string
	}{
		{"no claim", envReg, t.TempDir(), wrong},
		{"every claimed process gone", envReg, claimed(t, "fake"), nil},
		{"a session not collected", envReg, notCollected, wrong},
		{"an agent whose exporter ignores its environment", agents.New(agentstest.Agent{ID: "fake"}), claimed(t, "fake"), wrong},
		{"an agent this build does not know", envReg, claimed(t, "other"), wrong},
		{"a session only a commit claimed", envReg, gitOnly, map[int][]string{hostPID: {"HOME=/home/dev"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if c := AgentEnvCheck(tc.reg, tc.stateDir, relay, processes(tc.table), time.Now()); c.Status != Skip {
				t.Fatalf("check = %+v, want skip", c)
			}
		})
	}
}

// A running agent whose environment cannot be read alone is a skip that says so, not a pass.
func TestAgentEnvCheckSkipsAnUnreadableAgent(t *testing.T) {
	t.Parallel()
	reg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	table := map[int][]string{agentPID: nil, hostPID: {"HOME=/home/dev"}}
	c := AgentEnvCheck(reg, claimed(t, "fake"), Relay{Addr: "127.0.0.1:43180", Token: relayToken}, processes(table), time.Now())
	if c.Status != Skip || !strings.Contains(c.Detail, "cannot read") {
		t.Fatalf("check = %+v, want a skip saying the environment cannot be read", c)
	}
	// Beside a readable one (a shell on macOS is not), the readable one is judged.
	dir := claimed(t, "fake")
	claim.Write(dir, "s2", claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: []int{102, 202}}, time.Now())
	table[202] = []string{"HOME=/home/dev"}
	c = AgentEnvCheck(reg, dir, Relay{Addr: "127.0.0.1:43180", Token: relayToken}, processes(table), time.Now())
	if c.Status != Pass {
		t.Fatalf("check = %+v, want a pass on the readable agent", c)
	}
}

// Once a session has ended, its agent has exited and the nearest running process is its
// launcher, which is not judged: even where a subagent's claim, which never ends, names the
// same processes, and after a commit made by hand. The same agent process claimed again
// later, for a new session, is.
func TestAgentEnvCheckSkipsAnEndedSession(t *testing.T) {
	t.Parallel()
	reg := agents.New(envAgent{agentstest.Agent{ID: "fake"}})
	relay := Relay{Addr: "127.0.0.1:43180", Token: relayToken}
	now := time.Now()
	dir := t.TempDir()
	chain := []int{hookPID, agentPID, hostPID}
	claim.Write(dir, "subagent", claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: chain}, now.Add(-time.Minute))
	claim.Write(dir, "s1", claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: chain}, now.Add(-time.Minute))
	claim.End(dir, "s1", now.Add(-time.Second))
	// The host passes no exporter variable of its own: judged as the agent, it would pass.
	exited := processes(map[int][]string{hostPID: {"HOME=/home/dev"}})
	if c := AgentEnvCheck(reg, dir, relay, exited, now); c.Status != Skip {
		t.Fatalf("check = %+v, want a skip: the agent has exited", c)
	}
	// The developer then commits the agent's work from the launcher's shell: hook script 400, git 401.
	claim.Write(dir, "s1", claim.Claim{ProjectID: "p1", Tool: "fake", PIDs: []int{400, 401, hostPID}}, now)
	if c := AgentEnvCheck(reg, dir, relay, exited, now); c.Status != Skip {
		t.Fatalf("check after a commit = %+v, want a skip: the agent has exited", c)
	}
	// The agent cleared its conversation: one session ended, another runs in the same process.
	claim.Write(dir, "s2", claim.Claim{ProjectID: "p1", Tool: "fake", Cwd: "/work/a", PIDs: []int{101, agentPID, hostPID}}, now.Add(time.Second))
	running := processes(map[int][]string{agentPID: {"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"}, hostPID: {"HOME=/home/dev"}})
	if c := AgentEnvCheck(reg, dir, relay, running, now.Add(time.Second)); c.Status != Warn || !strings.Contains(c.Detail, "fake (pid 200)") {
		t.Fatalf("check = %+v, want the running agent judged", c)
	}
}
