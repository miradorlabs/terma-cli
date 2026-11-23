package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// AgentHooksCheck reports whether each agent the developer chose (mine) that reports
// through its hooks alone, with no exporter for the relay, still has the machine-wide hooks
// setup writes: without them nothing of that agent is recorded, and no other check notices.
// The agent's own planner judges it: the hooks are in place when installing them again,
// for terma at exe or for another path to the same file, would change nothing. Hooks that
// run another terma that is there still work, and only warn. False when no such agent was
// chosen.
func AgentHooksCheck(ctx context.Context, reg *agents.Registry, mine []string, exe string) (Check, bool) {
	var chosen []agents.UserHooks
	for _, a := range reg.With[agents.UserHooks]() {
		if _, exports := a.(agents.RelayExporter); !exports && reg.IsSupported(a.Name()) &&
			slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) }) {
			chosen = append(chosen, a)
		}
	}
	if len(chosen) == 0 {
		return Check{}, false
	}
	if exe == "" {
		return Check{Key: KeyAgentHooks, Status: Warn, Fix: "terma setup",
			Detail: "could not tell which terma this is, so terma's hooks were not checked for " + names(ctx, chosen)}, true
	}
	var ok, others, missing []string
	for _, a := range chosen {
		label := name(ctx, a)
		switch verdict, detail := judgeAgentHooks(a, exe); verdict {
		case hooksInPlace:
			ok = append(ok, label)
		case hooksRunOther:
			others = append(others, label+" (they run "+detail+")")
		default:
			missing = append(missing, label+" ("+detail+")")
		}
	}
	switch {
	case len(missing) > 0:
		return Check{Key: KeyAgentHooks, Status: Fail, Fix: "terma setup",
			Detail: "terma's hooks are missing, switched off or out of date for " + strings.Join(missing, ", ")}, true
	case len(others) > 0:
		return Check{Key: KeyAgentHooks, Status: Warn, Fix: "terma setup, to point them at this terma",
			Detail: "terma's hooks run another terma for " + strings.Join(others, ", ")}, true
	}
	return Check{Key: KeyAgentHooks, Status: Pass, Detail: "terma's hooks run for " + strings.Join(ok, ", ")}, true
}

type hooksVerdict int

const (
	hooksMissing hooksVerdict = iota
	hooksInPlace
	hooksRunOther
)

// judgeAgentHooks says whether a's hooks are in place for terma at exe, or for another
// terma its file names, which detail then gives; for missing hooks, detail says where.
func judgeAgentHooks(a agents.UserHooks, exe string) (hooksVerdict, string) {
	path, err := a.UserHooksPath()
	if err != nil {
		return hooksMissing, err.Error()
	}
	dir := filepath.Dir(path)
	plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(exe), true)
	if err != nil {
		return hooksMissing, err.Error()
	}
	if plan.Empty() {
		return hooksInPlace, ""
	}
	data, _ := os.ReadFile(path)
	for _, other := range hookmgr.TermaPaths(data) {
		if other == exe {
			continue
		}
		if plan, err := a.PlanUserHooks(dir, hookmgr.UserHookCommand(other), true); err != nil || !plan.Empty() {
			continue
		}
		if sameFile(other, exe) {
			return hooksInPlace, ""
		}
		if executable(other) {
			return hooksRunOther, output.TildePath(other)
		}
	}
	return hooksMissing, output.TildePath(path)
}

// name is the agent's display name with the version installed here, when it says.
func name(ctx context.Context, a agents.UserHooks) string {
	if v, ok := a.(agents.VersionReporter); ok {
		if version := v.DetectedVersion(ctx); version != "" {
			return a.DisplayName() + " " + version
		}
		return a.DisplayName() + " (version unknown)"
	}
	return a.DisplayName()
}

func names(ctx context.Context, as []agents.UserHooks) string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, name(ctx, a))
	}
	return strings.Join(out, ", ")
}

func sameFile(a, b string) bool {
	x, errA := os.Stat(a)
	y, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(x, y)
}

// executable reports a regular file the hook's `[ -x ]` guard would run; Windows marks no
// file executable.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}
