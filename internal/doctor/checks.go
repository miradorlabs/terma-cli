package doctor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// AgentHooksCheck reports, for the agents wired in this repository, whether their hooks
// are in place and whether the agent will run them; doctor and status share it.
//
// A wired agent is checked whoever uses it, an unwired one is missing only when named in
// mine, and trust is judged only for the developer's own agents (mine; empty means all).
func AgentHooksCheck(reg *agents.Registry, root string, mine []string) Check {
	var parts []string
	var fix string
	ready, of := 0, 0
	status := Pass
	problem := func(f string) {
		status = Warn
		if fix == "" {
			fix = f
		}
	}
	for _, a := range reg.All() {
		if a.HooksPath() == "" {
			continue
		}
		// A surface is wired through its agent's hooks file.
		named := slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) })
		if !agents.Wired(root, a) {
			if named {
				of++
				parts = append(parts, a.DisplayName()+" hooks missing")
				problem("terma install")
			}
			continue
		}
		used := len(mine) == 0 || named
		if used {
			of++
		}
		plan, err := a.Plan(root, true)
		if err != nil {
			parts = append(parts, a.DisplayName()+" hooks could not be read: "+err.Error())
			problem("repair " + a.HooksPath() + "; then run terma install")
			continue
		}
		if !plan.Empty() {
			parts = append(parts, a.DisplayName()+" hooks out of date")
			problem("terma update --refresh")
			continue
		}
		part := a.DisplayName() + " hooks present"
		trusting, gated := a.(agents.Trusting)
		if !gated || !used {
			if used {
				ready++
			}
			parts = append(parts, part)
			continue
		}
		trust, err := trusting.Trust(root)
		switch {
		case err != nil:
			// Unreadable is not untrusted.
			part += " (could not read " + a.DisplayName() + "'s trust record: " + err.Error() + ")"
			ready++
		case !trust.Trusted:
			part += trust.Detail
			problem(trust.Fix)
		default:
			part += trust.Detail
			ready++
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return Check{Status: Skip, Detail: "no agent hooks are wired in this repository"}
	}
	return Check{Status: status, Detail: strings.Join(parts, "; "), Fix: fix, Ready: ready, Of: of}
}

// stateCheck reports saved state this build has not finished migrating.
func stateCheck() (Check, bool) {
	dir, err := config.Dir()
	if err != nil || !migrate.Pending(dir) {
		return Check{}, false
	}
	s, err := migrate.Load(dir)
	switch {
	case err != nil:
		return Check{Status: Warn, Detail: "the migration record cannot be read: " + err.Error(), Fix: "terma update --refresh"}, true
	case s.Failed != nil:
		return Check{Status: Fail, Detail: fmt.Sprintf("%s failed: %s", s.Failed.Name, s.Failed.Error), Fix: "terma update --refresh"}, true
	}
	return Check{Status: Warn, Detail: fmt.Sprintf("%d migration(s) from this update not applied yet", migrate.Remaining(s)), Fix: "terma update --refresh"}, true
}

// HooksCheck is doctor's wording for the commit-hook verdict.
func HooksCheck(w HookWiring) Check {
	switch {
	case w.Err != nil:
		return Check{Status: Fail, Detail: w.Err.Error(), Fix: "terma install"}
	case w.Changes > 0 && w.Stale == w.Changes:
		return Check{Status: Fail, Detail: fmt.Sprintf("%s wiring was written by an earlier terma (%d file(s) out of date)", w.Manager, w.Stale), Fix: "terma update --refresh"}
	case w.Changes > 0:
		return Check{Status: Fail, Detail: fmt.Sprintf("%s wiring is missing (%d file change(s))", w.Manager, w.Changes), Fix: "terma install"}
	case w.Unpointed:
		return Check{Status: Fail, Detail: "shims are committed but git is not pointed at them in this clone (core.hooksPath=" + cmp.Or(w.HooksPath, "unset") + ")", Fix: "terma install"}
	case w.Manager == hookmgr.GitShim:
		return Check{Status: Pass, Detail: string(w.Manager) + " shims, core.hooksPath set"}
	}
	return Check{Status: Pass, Detail: string(w.Manager)}
}

// StatusLineCheck is doctor's wording for the status-line verdict.
func StatusLineCheck(v StatusLineVerdict) Check {
	switch v.Capture {
	case StatusLineUnknown:
		return Check{Status: Warn, Detail: v.Err.Error()}
	case StatusLineOverridden:
		return Check{Status: Warn,
			Detail: "overridden by " + strings.Join(v.Overrides, ", ") + "; plan usage is not captured in this repository",
			Fix:    "remove statusLine from that file, or accept that this repository does not report plan usage"}
	case StatusLineBehind:
		return Check{Status: Pass, Detail: "capturing plan usage; " + output.SanitizeTerminal(v.Renderer) + " runs behind it"}
	case StatusLineDefault:
		return Check{Status: Pass, Detail: "capturing plan usage (terma's default line)"}
	case StatusLineReplaced:
		return Check{Status: Warn, Detail: "replaced by your own status line since terma wrapped it; plan usage is not captured", Fix: "terma install"}
	}
	return Check{Status: Warn, Detail: "not wrapped; plan usage is not captured", Fix: "terma install"}
}

// HarnessCheck folds every agent's verdict into doctor's one export check.
func HarnessCheck(reg *agents.Registry, verdicts []HarnessVerdict, otlpURL, projectID string, bound bool) (check Check) {
	// Count working agents independently of another agent's failure.
	defer func() {
		check.Of = len(verdicts)
		for _, v := range verdicts {
			if v.Reaches(bound) {
				check.Ready++
			}
		}
	}()
	var problems, fixes []string
	for _, v := range verdicts {
		if v.EmissionProblem != "" {
			problems = append(problems, v.DisplayName+": "+v.EmissionProblem)
			if !slices.Contains(fixes, v.EmissionFix) {
				fixes = append(fixes, v.EmissionFix)
			}
		}
	}
	if len(problems) > 0 {
		return Check{Status: Fail, Detail: strings.Join(problems, "; "), Fix: strings.Join(fixes, "; ")}
	}
	var connected, installed, repoDecides, silent []string
	for _, v := range verdicts {
		installed = append(installed, v.DisplayName)
		switch v.Route {
		case RouteOtherProject:
			return Check{Status: Fail, Detail: v.DisplayName + " reports to team " + v.OtherProject + ", not " + projectID, Fix: "terma install"}
		case RouteHooks:
			connected = append(connected, v.DisplayName+" (repository hooks)")
		case RouteGlobal:
			connected = append(connected, v.DisplayName)
		case RouteRepoDecides:
			connected = append(connected, v.DisplayName)
			repoDecides = append(repoDecides, v.DisplayName)
			// An agent that cannot carry a repository policy is as silent as one without.
			if !v.RepoAsks {
				silent = append(silent, v.DisplayName)
			}
		}
	}
	if len(installed) == 0 {
		return Check{Status: Fail, Detail: "no coding agent found (" + agents.DisplayNames(reg.Supported()) + ")", Fix: "install one, then terma install"}
	}
	if len(connected) == 0 {
		return Check{Status: Fail, Detail: strings.Join(installed, ", ") + " installed but not exporting to " + otlpURL, Fix: "terma install"}
	}
	detail := strings.Join(connected, ", ") + " → " + otlpURL
	if len(repoDecides) == 0 {
		return Check{Status: Pass, Detail: detail}
	}
	qualifier := "only where a repository asks"
	if len(repoDecides) < len(connected) {
		qualifier = strings.Join(repoDecides, ", ") + ": " + qualifier
	}
	detail += " (" + qualifier + ")"
	if !bound {
		return Check{Status: Pass, Detail: detail}
	}
	if len(silent) == 0 {
		return Check{Status: Pass, Detail: detail + "; this repository asks"}
	}
	return Check{
		Status: Fail,
		Detail: detail + "; this repository does not route " + strings.Join(silent, ", ") + " to its team, so its sessions send nothing",
		Fix:    "terma install",
	}
}

// RelayCheck is doctor's "agent exporting to Terma" through the local relay: its address
// is free, it delivers to env (this profile's environment), the developer's agents send to
// it, its service is this terma's, and this repository is bound and keyed.
func RelayCheck(reg *agents.Registry, relay Relay, keys Keys, projectID, env string, selected []string) Check {
	if relay.Err != nil {
		return Check{Status: Fail, Detail: relay.Err.Error()}
	}
	dir, addr, running := relay.Dir, relay.Addr, relay.Running
	if !running && relay.Squatted {
		return Check{Status: Fail, Detail: "another process is listening on " + addr + " and receives the agents' telemetry",
			Fix: "stop it, or move the relay with `terma relay setup --addr`"}
	}
	// A relay started without this profile's environment holds no key for its teams, and
	// drops everything it receives.
	if running && relay.Environment != "" && env != "" && relay.Environment != env {
		return Check{Status: Fail,
			Detail: "the local relay on " + addr + " delivers to the " + relay.Environment + " environment, not this profile's " + env + ", so it forwards none of this profile's sessions",
			Fix:    "terma install"}
	}
	mine := func(e agents.Agent) bool {
		if len(selected) == 0 {
			return reg.IsSupported(e.Name())
		}
		return slices.ContainsFunc(agents.Selections(e), func(s string) bool { return slices.Contains(selected, s) })
	}
	var wrong []string
	for _, e := range reg.With[agents.RelayExporter]() {
		if pointed, known := e.RelayPointed(addr); mine(e) && known && !pointed {
			wrong = append(wrong, e.DisplayName())
		}
	}
	if len(wrong) > 0 {
		return Check{Status: Fail, Detail: strings.Join(wrong, " and ") + " not exporting to the local relay", Fix: "terma relay setup"}
	}
	if relay.ServiceInstalled && !relay.ServiceCurrent {
		return Check{Status: Warn,
			Detail: "the relay service was written by an earlier terma, or for another binary or environment, so the system may not start this relay",
			Fix:    "terma install"}
	}
	if relay.ServiceInstalled && running && relay.HookStarted {
		return Check{Status: Warn,
			Detail: "a relay a hook started holds " + addr + ", so the relay service waits behind it and misses what agents export before their first hook",
			Fix:    "terma install"}
	}
	for _, e := range reg.With[agents.RelayExporter]() {
		if c, ok := e.(agents.RelayChecker); ok && mine(e) {
			if detail, fix, problem := c.RelayProblem(dir); problem {
				return Check{Status: Warn, Detail: detail, Fix: fix}
			}
		}
	}
	state := "starts with the next hook"
	if running {
		state = "running"
	}
	switch {
	case projectID == "":
		return Check{Status: Warn, Detail: "local relay on " + addr + " (" + state + "); this repository is not bound, so its sessions are never forwarded", Fix: "terma install"}
	case !keys.has("", projectID) && !slices.ContainsFunc(reg.With[agents.RelayExporter](), func(e agents.RelayExporter) bool { return keys.has(e.Name(), projectID) }):
		return Check{Status: Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this team on this machine, so its sessions are dropped", Fix: "terma install"}
	}
	return Check{Status: Pass, Detail: "through the local relay on " + addr + " (" + state + "); only this repository's sessions are forwarded"}
}
