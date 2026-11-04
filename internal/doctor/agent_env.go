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

// exporterVars are one OpenTelemetry SDK exporter's variables and the route they send to on
// the relay.
type exporterVars struct{ endpoint, headers, protocol, route string }

// exporterSet is the SDK's generic variables, whose endpoint each signal's route is appended
// to, then each signal's own, whose endpoint is sent to as it is.
var exporterSet = []exporterVars{
	{harness.EnvOTLPEndpoint, harness.EnvOTLPHeaders, harness.EnvOTLPProtocol, ""},
	{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "/v1/logs"},
	{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "/v1/traces"},
	{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "/v1/metrics"},
}

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
	case len(procs) == 0 && len(unread) > 0:
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
// names each agent with one running that could not be read. A claim lists the ancestors of each hook that ran
// for the session, nearest first, one chain after another; a hook's own processes exit with
// it, so the nearest process of each run of running ones is an agent, or what a git hook
// ran under (a developer's shell), or an exited process's id that another has taken since.
// All are returned, so the agent always is; the others add a finding only when they too
// started with what a check flags.
func runningAgents(reg *agents.Registry, stateDir string, environ Environ, now time.Time) (procs map[int]agentProc, unread map[string]bool) {
	procs, unread = map[int]agentProc{}, map[string]bool{}
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
				unread[a.Name()] = true
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
	// A token leading a relay endpoint's path authorizes in place of the header.
	onPath := token != "" && slices.ContainsFunc(exporterSet, func(e exporterVars) bool {
		_, relay := relayPath(vars[e.endpoint], addr)
		return relay && strings.Contains(vars[e.endpoint], token)
	})
	var out []string
	for _, e := range exporterSet {
		v, set := vars[e.endpoint]
		path, relay := relayPath(v, addr)
		generic := e.route == ""
		doubled := generic && signalPath(v)
		switch {
		case !set:
		case !relay:
			out = append(out, e.endpoint+" sends elsewhere than the local relay")
		case !doubled && !relayRoute(path, e.route, token):
			out = append(out, e.endpoint+" names a path the local relay does not serve")
		}
		if doubled {
			out = append(out, e.endpoint+" ends in a signal path the exporter appends again")
		}
		if strings.Contains(strings.ToLower(vars[e.protocol]), "grpc") {
			out = append(out, e.protocol+" picks gRPC, which the local relay does not serve")
		}
		// A signal's headers merge over the generic ones: only an Authorization of their own replaces the token.
		h, set := vars[e.headers]
		if set && (generic || authorizes(h)) && token != "" && !onPath && !strings.Contains(h, token) {
			out = append(out, e.headers+" lacks the relay's current token")
		}
	}
	return strings.Join(out, ", ")
}

// relayPath returns the path of raw when it is plain HTTP to the relay at addr.
func relayPath(raw, addr string) (string, bool) {
	u, err := url.Parse(strings.Trim(raw, `"'`))
	if err != nil || addr == "" || u.Scheme != "http" || u.Host != addr {
		return "", false
	}
	return u.Path, true
}

// signalPath reports whether the URL raw ends in a signal's route.
func signalPath(raw string) bool {
	u, err := url.Parse(strings.Trim(raw, `"'`))
	return raw != "" && err == nil && slices.ContainsFunc(exporterSet, func(e exporterVars) bool {
		return e.route != "" && strings.HasSuffix(strings.TrimRight(u.Path, "/"), e.route)
	})
}

// relayRoute reports whether path is route on the relay, alone or after the relay's token;
// with the token unknown, after any one segment. The generic endpoint's route is "".
func relayRoute(path, route, token string) bool {
	if route == "" {
		path = strings.TrimRight(path, "/")
	}
	if path == route {
		return true
	}
	first, rest, found := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if found {
		rest = "/" + rest
	}
	return first != "" && (token == "" || first == token) && rest == route
}

// authorizes reports whether an OTLP headers value sets an Authorization header.
func authorizes(headers string) bool {
	for pair := range strings.SplitSeq(headers, ",") {
		if k, _, ok := strings.Cut(pair, "="); ok && strings.EqualFold(strings.TrimSpace(k), "authorization") {
			return true
		}
	}
	return false
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
