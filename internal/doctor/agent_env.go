package doctor

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// exporterEndpoints are the OpenTelemetry SDK's endpoint variables, the generic one first.
var exporterEndpoints = []string{harness.EnvOTLPEndpoint,
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"}

// signalPaths are what an exporter appends to the generic endpoint, one per signal.
var signalPaths = []string{"/v1/logs", "/v1/traces", "/v1/metrics"}

// envFix says where an agent's environment comes from and that it is read only at startup.
const envFix = "remove those variables where they are set: the process that started the agent " +
	"(an IDE or agent host, often from a config file of its own; `ps -o ppid= -p <pid>` names it) or a shell profile; " +
	"then restart the agent, which reads its exporter settings only when it starts"

// agentProc is a running agent process and the environment it started with, nil unread.
type agentProc struct {
	agent agents.Agent
	env   []string
}

// AgentEnvCheck judges the environment each running agent process of a collected session
// started with (runningAgents), against the relay: an exporter variable there overrides the
// settings files the export check reads. Only variable names are said, never a value: an
// endpoint or header may hold a credential.
func AgentEnvCheck(reg *agents.Registry, stateDir string, relay Relay, environ Environ, now time.Time) Check {
	procs, unread := runningAgents(reg, stateDir, environ, now)
	switch {
	case len(procs) == 0 && unread:
		return Check{Status: Skip, Detail: "terma cannot read the environment of the running agent processes on this system"}
	case len(procs) == 0:
		return Check{Status: Skip, Detail: "no agent process of a collected session is running"}
	}
	type finding struct{ agent, what string }
	found := map[finding][]int{}
	var names []string
	for _, pid := range slices.Sorted(maps.Keys(procs)) {
		p := procs[pid]
		if !slices.Contains(names, p.agent.DisplayName()) {
			names = append(names, p.agent.DisplayName())
		}
		if what := exporterEnvFindings(p.env, relay.Addr, relay.Token); what != "" {
			f := finding{p.agent.DisplayName(), what}
			found[f] = append(found[f], pid)
		}
	}
	if len(found) == 0 {
		return Check{Status: Pass, Detail: fmt.Sprintf("%s started with no exporter variable that overrides the local relay on %s",
			strings.Join(names, ", "), relay.Addr)}
	}
	var parts []string
	for _, f := range slices.SortedFunc(maps.Keys(found), func(a, b finding) int { return strings.Compare(a.agent+a.what, b.agent+b.what) }) {
		parts = append(parts, f.agent+" ("+pidList(found[f])+") started with exporter variables that override terma's setup: "+f.what)
	}
	return Check{Status: Warn, Detail: strings.Join(parts, "; "), Fix: envFix}
}

// runningAgents are the running processes of agents whose exporter reads its startup
// environment, in collected sessions claimed under stateDir, readable ones by pid; unread
// says one ran that could not be read. A claim lists the ancestors of each hook that ran
// for the session, nearest first, one chain after another; a hook's own processes exit with
// it, so the nearest process of each run of running ones is an agent, or what a git hook
// ran under (a developer's shell), or an exited process's id that another has taken since.
// All are returned, so the agent always is; the others add a finding only when they too
// started with what a check flags.
func runningAgents(reg *agents.Registry, stateDir string, environ Environ, now time.Time) (procs map[int]agentProc, unread bool) {
	procs = map[int]agentProc{}
	for _, c := range claim.List(stateDir, now) {
		a, ok := reg.ForTool(c.Tool)
		if _, exports := a.(agents.EnvExporter); !ok || !exports || c.ProjectID == "" {
			continue
		}
		after := false
		for _, pid := range c.PIDs {
			env, alive := environ(pid)
			switch {
			case alive && !after && env == nil:
				unread = true
			case alive && !after:
				procs[pid] = agentProc{a, env}
			}
			after = alive
		}
	}
	return procs, unread
}

// exporterEnvFindings names, by variable, what in env keeps an exporter from the relay at
// addr, whose token is token; "" when nothing does.
func exporterEnvFindings(env []string, addr, token string) string {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "OTEL_EXPORTER_OTLP_") && v != "" {
			vars[k] = v
		}
	}
	var out []string
	for _, key := range exporterEndpoints {
		if v, ok := vars[key]; ok && !relayEndpoint(v, addr) {
			out = append(out, key+" sends elsewhere than the local relay")
		}
	}
	generic := vars[harness.EnvOTLPEndpoint]
	if u, err := url.Parse(generic); generic != "" && err == nil {
		path := strings.TrimRight(u.Path, "/")
		if slices.ContainsFunc(signalPaths, func(s string) bool { return strings.HasSuffix(path, s) }) {
			out = append(out, harness.EnvOTLPEndpoint+" ends in a signal path the exporter appends again")
		}
	}
	// A token leading an endpoint's path authorizes in place of the header.
	onPath := slices.ContainsFunc(exporterEndpoints, func(key string) bool {
		return relayEndpoint(vars[key], addr) && strings.Contains(vars[key], token)
	})
	if h, ok := vars[harness.EnvOTLPHeaders]; ok && token != "" && !strings.Contains(h, token) && !onPath {
		out = append(out, harness.EnvOTLPHeaders+" lacks the relay's current token")
	}
	return strings.Join(out, ", ")
}

// relayEndpoint reports whether raw is plain HTTP to the relay at addr.
func relayEndpoint(raw, addr string) bool {
	u, err := url.Parse(strings.Trim(raw, `"'`))
	return err == nil && addr != "" && u.Scheme == "http" && u.Host == addr
}

func pidList(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	if len(s) == 1 {
		return "pid " + s[0]
	}
	return "pids " + strings.Join(s, ", ")
}
