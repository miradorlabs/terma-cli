package doctor

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// TracesCheck says whether each of the developer's agents (mine; empty means all) whose
// spans need switches of their own exports them: by the settings that apply at root, and,
// with environ, by the environment its running processes started with (runningAgents).
// Spans alone carry some of what Terma shows, and nothing else reports their absence. False
// when no such agent is installed.
func TracesCheck(ctx context.Context, reg *agents.Registry, mine []string, root, stateDir string, environ Environ, now time.Time) (Check, bool) {
	var procs map[int]agentProc
	if environ != nil {
		procs, _ = runningAgents(reg, stateDir, environ, now)
	}
	var exporting, parts, fixes []string
	fix := func(f string) {
		if !slices.Contains(fixes, f) {
			fixes = append(fixes, f)
		}
	}
	for _, a := range reg.With[agents.TraceGated]() {
		if len(mine) > 0 && !slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) }) || !a.Installed(ctx) {
			continue
		}
		var problems []string
		if off := a.TracesOff(root, nil); len(off) > 0 {
			problems = append(problems, "its settings lack "+strings.Join(off, ", "))
			fix("terma setup (it writes both into the agent's settings), then restart the agent")
		}
		for _, pid := range slices.Sorted(maps.Keys(procs)) {
			if procs[pid].agent.Name() != a.Name() {
				continue
			}
			if off := a.TracesOff("", procs[pid].env); len(off) > 0 {
				problems = append(problems, strings.Join(off, ", ")+" in the environment pid "+strconv.Itoa(pid)+" started with switches them off")
				fix("remove that variable where it is set (the IDE or agent host that started the agent, or a shell profile), then restart the agent")
			}
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
	case len(exporting) > 0:
		return Check{Status: Pass, Detail: strings.Join(exporting, ", ") + " exports its spans"}, true
	}
	return Check{}, false
}
