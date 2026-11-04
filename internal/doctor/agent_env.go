package doctor

import (
	"cmp"
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

// AgentEnvCheck judges the environment each running agent process of a collected session
// started with, against the relay: an exporter variable there overrides the settings files
// the export check reads. A claim under stateDir lists the ancestors of each hook that ran
// for the session, nearest first, one chain after another; a hook's own processes exit with
// it, so the nearest process of each run of running ones is an agent, or what a git hook
// ran under (a developer's shell), or an exited process's id that another has taken since.
// All are judged, so the agent always is; the others add a finding only when they too
// started with exporter variables that bypass the relay. A process whose latest claim is
// a session's end is not judged: once the agent exits, the nearest running process is its
// launcher. Only variable names are said, never a value: an endpoint or header may hold a
// credential.
//
// Accepted risk: an agent that exits without its end hook completing (a crash, a kill, the
// hook's timeout) leaves its claim fresh for claim.TTL after the session's last hook, a
// commit's included, so its launcher is judged as the agent and a launcher that sets no
// exporter variable of its own reads as a pass. Telling the agent's own process apart (by
// its executable) would close it.
func AgentEnvCheck(reg *agents.Registry, stateDir string, relay Relay, environ Environ, now time.Time) Check {
	type proc struct {
		agent string
		env   []string
		at    time.Time
		ended bool
	}
	seen := map[int]proc{}
	for _, c := range claim.List(stateDir, now) {
		a, ok := reg.ForTool(c.Tool)
		// A session only git hooks claimed (a commit after its claim expired) names no agent process.
		if _, exports := a.(agents.EnvExporter); !ok || !exports || c.ProjectID == "" || !c.Agent {
			continue
		}
		// A subagent's claim names its session's processes but never ends: the later word wins.
		at, ended := c.ClaimedAt, !c.EndedAt.IsZero()
		if ended {
			at = c.EndedAt
		}
		after := false
		for _, pid := range c.PIDs {
			env, alive := environ(pid)
			if prev, ok := seen[pid]; alive && !after && (!ok || at.After(prev.at)) {
				seen[pid] = proc{a.DisplayName(), env, at, ended}
			}
			after = alive
		}
	}
	procs := map[int]proc{}
	unread := false
	for pid, p := range seen {
		switch {
		case p.ended:
		case p.env == nil:
			unread = true
		default:
			procs[pid] = p
		}
	}
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
		if !slices.Contains(names, p.agent) {
			names = append(names, p.agent)
		}
		if what := exporterEnvFindings(p.env, relay.Addr, relay.Token); what != "" {
			f := finding{p.agent, what}
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
	}
	if token == "" {
		return strings.Join(out, ", ")
	}
	// Each signal is authorized on its own: by a token leading the path of the endpoint it
	// sends to, else by the bearer of the headers it sends, its own Authorization replacing
	// the generic one.
	generic := exporterSet[0]
	for _, e := range exporterSet[1:] {
		endpoint := cmp.Or(vars[e.endpoint], vars[generic.endpoint])
		if path, relay := relayPath(endpoint, addr); relay && strings.HasPrefix(path, "/"+token+"/") {
			continue
		}
		headers := e.headers
		if bearer(vars[headers]) == "" {
			headers = generic.headers
		}
		if h, set := vars[headers]; set && bearer(h) != token && !slices.Contains(out, headers+" lacks the relay's current token") {
			out = append(out, headers+" lacks the relay's current token")
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

// bearer is the credential an OTLP headers value's Authorization presents as "Bearer <x>",
// as the relay reads it; "" when it sets none.
func bearer(headers string) string {
	for pair := range strings.SplitSeq(strings.Trim(headers, `"'`), ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "authorization") {
			continue
		}
		// The SDK URL-decodes header values.
		if decoded, err := url.QueryUnescape(strings.TrimSpace(v)); err == nil {
			v = decoded
		}
		if got, ok := strings.CutPrefix(strings.TrimSpace(v), "Bearer "); ok {
			return strings.TrimSpace(got)
		}
		return ""
	}
	return ""
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
