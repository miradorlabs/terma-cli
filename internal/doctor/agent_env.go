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

// AgentEnvCheck judges the environment each running agent process of a collected session
// started with, against the relay: an exporter variable there overrides the settings files
// the export check reads. A claim under stateDir lists its hooks' ancestors, nearest first,
// each run's appended last, and the processes a running agent descends from all run; so
// the nearest of the running processes that end the list is the latest run's agent, never
// an exited run's process whose id another has taken since. Once that agent exits, its host
// is judged until the claim expires. Only variable names are said, never a value: an
// endpoint or header may hold a credential.
func AgentEnvCheck(reg *agents.Registry, stateDir string, relay Relay, environ Environ, now time.Time) Check {
	type proc struct {
		agent string
		env   []string
	}
	procs := map[int]proc{}
	for _, c := range claim.List(stateDir, now) {
		a, ok := reg.ForTool(c.Tool)
		if _, exports := a.(agents.EnvExporter); !ok || !exports || c.ProjectID == "" {
			continue
		}
		var nearest proc
		pid := 0
		for i := len(c.PIDs) - 1; i >= 0; i-- {
			env, alive := environ(c.PIDs[i])
			if !alive {
				break
			}
			pid, nearest = c.PIDs[i], proc{a.DisplayName(), env}
		}
		if pid != 0 {
			procs[pid] = nearest
		}
	}
	unread := 0
	for pid, p := range procs {
		if p.env == nil {
			unread++
			delete(procs, pid)
		}
	}
	switch {
	case len(procs) == 0 && unread > 0:
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
		running := "1 running process"
		if len(procs) > 1 {
			running = fmt.Sprintf("%d running processes", len(procs))
		}
		return Check{Status: Pass, Detail: fmt.Sprintf("%s (%s) started with no exporter variable that overrides the local relay on %s",
			strings.Join(names, ", "), running, relay.Addr)}
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
	onPath := slices.ContainsFunc(exporterEndpoints, func(key string) bool { return strings.Contains(vars[key], token) })
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
