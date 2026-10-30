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
	if !claim.Write(dir, "s1", claim.Claim{ProjectID: "p1", Tool: tool, PIDs: []int{hookPID, agentPID, hostPID}}, time.Now()) {
		t.Fatal("claim not written")
	}
	return dir
}

// processes is a fake process table: each live process's startup environment.
func processes(table map[int][]string) Environ {
	return func(pid int) ([]string, bool) {
		env, ok := table[pid]
		return env, ok
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
			status: Pass, want: []string{"fake (1 running process) started with no exporter variable", "127.0.0.1:43180"}},
		{name: "the relay with its token", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180",
			"OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer " + relayToken}, status: Pass},
		{name: "the token on the relay's path, no header", env: []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:43180/" + relayToken},
			status: Pass},
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
		claim.Write(dir, id, claim.Claim{ProjectID: "p1", Tool: "fake", PIDs: pids}, now)
	}
	bad := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=https://otel.example.com"}
	table := map[int][]string{201: bad, 202: bad, 203: {"HOME=/home/dev"}}
	c := AgentEnvCheck(reg, dir, Relay{Addr: "127.0.0.1:43180", Token: relayToken}, processes(table), now)
	if c.Status != Warn || !strings.Contains(c.Detail, "fake (pids 201, 202)") || strings.Contains(c.Detail, "203") {
		t.Fatalf("check = %+v, want a warning naming pids 201 and 202 together", c)
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
	for _, tc := range []struct {
		name     string
		reg      *agents.Registry
		stateDir string
		table    map[int][]string
	}{
		{"no claim", envReg, t.TempDir(), wrong},
		{"every claimed process gone or unreadable", envReg, claimed(t, "fake"), nil},
		{"a session not collected", envReg, notCollected, wrong},
		{"an agent whose exporter ignores its environment", agents.New(agentstest.Agent{ID: "fake"}), claimed(t, "fake"), wrong},
		{"an agent this build does not know", envReg, claimed(t, "other"), wrong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if c := AgentEnvCheck(tc.reg, tc.stateDir, relay, processes(tc.table), time.Now()); c.Status != Skip {
				t.Fatalf("check = %+v, want skip", c)
			}
		})
	}
}
