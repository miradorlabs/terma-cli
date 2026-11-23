package doctor

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// AgentHooksCheck reports whether each agent the developer chose (mine) that reports
// through its hooks alone, with no exporter for the relay, still has the machine-wide
// hooks setup writes for this terma, whose entries run command: without them nothing of
// that agent is recorded, and no other check notices. The agent's own planner judges it:
// the hooks are in place when installing them again would change nothing. False when no
// such agent was chosen, or command is unknown.
func AgentHooksCheck(reg *agents.Registry, mine []string, command func(event string) string) (Check, bool) {
	if command == nil {
		return Check{}, false
	}
	var ok, missing []string
	for _, a := range reg.With[agents.UserHooks]() {
		if _, exports := a.(agents.RelayExporter); exports || !reg.IsSupported(a.Name()) ||
			!slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) }) {
			continue
		}
		path, err := a.UserHooksPath()
		if err != nil {
			missing = append(missing, a.DisplayName()+" ("+err.Error()+")")
			continue
		}
		plan, err := a.PlanUserHooks(filepath.Dir(path), command, true)
		switch {
		case err != nil:
			missing = append(missing, a.DisplayName()+" ("+err.Error()+")")
		case !plan.Empty():
			missing = append(missing, a.DisplayName()+" ("+output.TildePath(path)+")")
		default:
			ok = append(ok, a.DisplayName())
		}
	}
	switch {
	case len(missing) > 0:
		return Check{Key: KeyAgentHooks, Status: Fail, Fix: "terma setup",
			Detail: "terma's hooks are missing or out of date, so no session is recorded, for " + strings.Join(missing, ", ")}, true
	case len(ok) > 0:
		return Check{Key: KeyAgentHooks, Status: Pass, Detail: strings.Join(ok, ", ") + " runs terma's hooks"}, true
	}
	return Check{}, false
}
