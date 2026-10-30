package doctor

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// TracesCheck says whether each of the developer's agents (mine; empty means all) whose
// spans need switches of their own exports them: by its settings and, with environ, by the
// environment its running processes started with (runningAgents). Spans alone carry some of
// what Terma shows, and nothing else reports their absence. False when no such agent is
// installed.
func TracesCheck(ctx context.Context, reg *agents.Registry, mine []string, stateDir string, environ Environ, now time.Time) (Check, bool) {
	var procs map[int]agentProc
	unread := false
	if environ != nil {
		procs, unread = runningAgents(reg, stateDir, environ, now)
	}
	pids := slices.Sorted(maps.Keys(procs))
	var exporting, parts, fixes []string
	fix := func(f string) {
		if !slices.Contains(fixes, f) {
			fixes = append(fixes, f)
		}
	}
	for _, a := range reg.With[agents.TraceGated]() {
		if len(mine) > 0 && !slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) }) {
			continue
		}
		if !a.Installed(ctx) {
			continue
		}
		var problems []string
		switch off, err := a.TracesOff(); {
		case err != nil:
			problems = append(problems, "terma cannot read its settings: "+err.Error())
			fix("repair the settings file named above, then restart the agent")
		case len(off) > 0:
			problems = append(problems, "its settings lack "+strings.Join(off, ", "))
			fix("terma setup (it writes them into the agent's settings), then restart the agent")
		}
		inEnv := map[string][]int{}
		for _, pid := range pids {
			if p := procs[pid]; p.agent.Name() == a.Name() {
				if off := a.TracesOffIn(p.env); len(off) > 0 {
					key := strings.Join(off, ", ")
					inEnv[key] = append(inEnv[key], pid)
				}
			}
		}
		for _, off := range slices.Sorted(maps.Keys(inEnv)) {
			problems = append(problems, off+" in the environment "+pidList(inEnv[off])+" started with switches them off")
			fix("remove that variable where it is set (the IDE or agent host that started the agent, or a shell profile), then restart the agent")
		}
		if len(problems) == 0 {
			exporting = append(exporting, a.DisplayName())
			continue
		}
		parts = append(parts, a.DisplayName()+" exports no spans ("+strings.Join(problems, "; ")+"), so Terma has no "+a.SpansCarry()+" for its calls")
	}
	switch {
	case len(parts) > 0:
		return Check{Status: Warn, Detail: strings.Join(parts, "; "), Fix: strings.Join(fixes, "; ")}, true
	case len(exporting) > 0 && unread:
		return Check{Status: Pass, Detail: strings.Join(exporting, ", ") + " exports its spans by its settings; terma cannot read the environment a running process of it started with"}, true
	case len(exporting) > 0:
		return Check{Status: Pass, Detail: strings.Join(exporting, ", ") + " exports its spans"}, true
	}
	return Check{}, false
}
