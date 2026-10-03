package doctor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// UserHooksCheck reports whether each of the developer's agents (mine; empty means all)
// runs terma's machine-wide hooks: one that gates them behind trust skips a changed entry
// in silence, and records nothing for it. A warning; false when no agent has any.
func UserHooksCheck(reg *agents.Registry, mine []string) (Check, bool) {
	var parts []string
	c := Check{Status: Pass}
	for _, a := range reg.All() {
		gated, ok := a.(agents.UserHooksTrust)
		if !ok || len(mine) > 0 && !slices.ContainsFunc(agents.Selections(a), func(s string) bool { return slices.Contains(mine, s) }) {
			continue
		}
		present, trusted, err := gated.UserHooksTrusted()
		switch {
		case !present && err == nil:
			continue
		case err != nil:
			// Unreadable is not untrusted.
			parts = append(parts, a.DisplayName()+": could not read its trust record: "+err.Error())
		case !trusted:
			parts = append(parts, a.DisplayName()+" skips some or all of them until you approve them")
			if c.Status != Warn {
				c.Status, c.Fix = Warn, gated.UserHooksTrustStep()
			}
		default:
			parts = append(parts, a.DisplayName()+" trusts them")
		}
	}
	if len(parts) == 0 {
		return Check{}, false
	}
	c.Detail = strings.Join(parts, "; ")
	return c, true
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
		return Check{Status: Warn, Detail: "the migration record cannot be read: " + err.Error(), Fix: "terma update"}, true
	case s.Failed != nil:
		return Check{Status: Fail, Detail: fmt.Sprintf("%s failed: %s", s.Failed.Name, s.Failed.Error), Fix: "terma update"}, true
	}
	return Check{Status: Warn, Detail: fmt.Sprintf("%d migration(s) from this update not applied yet", migrate.Remaining(s)), Fix: "terma update"}, true
}

// GlobalDestination says where global mode sends an unbound repository's sessions. Only a
// project this command resolved has a name here; the policy carries the team's id alone.
func GlobalDestination(cfg *config.Config) string {
	id := cmp.Or(cfg.Policy.DefaultProjectID, cfg.ProjectID)
	switch {
	case id != "" && id == cfg.ProjectID && cfg.ProjectName != "":
		return "its sessions report to " + cfg.ProjectName
	case id != "":
		return "its sessions report to the team chosen at setup (" + id + ")"
	}
	return "its sessions report to the team chosen at setup"
}

// HooksPathCheck is doctor's wording for where git looks for a repository's hooks: a
// local setting outranks terma's global hooks, and one naming no hooks runs none.
func HooksPathCheck(h HooksPath) Check {
	where := "core.hooksPath=" + h.Value + " (" + h.Scope + ")"
	switch {
	case h.Local() && h.Hookless:
		return Check{Status: Warn, Detail: where + " holds no hooks, so git runs none here", Fix: "git config --" + h.Scope + " --unset core.hooksPath, or restore the hooks it names"}
	case h.Local():
		return Check{Status: Warn, Detail: where + " outranks terma's global git hooks, so commits here are not stamped",
			Fix: "git config --" + h.Scope + " --unset core.hooksPath, if nothing else in this repository needs it"}
	case h.TermaGlobal && !h.Hookless:
		return Check{Status: Pass, Detail: "terma's global git hooks"}
	}
	return Check{Status: Warn, Detail: "terma's global git hooks are not in effect (" + cmp.Or(h.Value, "core.hooksPath unset") + "), so commits here are not stamped", Fix: "terma setup"}
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
		return Check{Status: Warn, Detail: "replaced by your own status line since terma wrapped it; plan usage is not captured", Fix: "terma setup"}
	}
	return Check{Status: Warn, Detail: "not wrapped; plan usage is not captured", Fix: "terma setup"}
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
			return Check{Status: Fail, Detail: v.DisplayName + " reports to team " + v.OtherProject + ", not " + projectID, Fix: "terma setup"}
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
		return Check{Status: Fail, Detail: "no coding agent found (" + agents.DisplayNames(reg.Supported()) + ")", Fix: "install one, then terma setup"}
	}
	if len(connected) == 0 {
		return Check{Status: Fail, Detail: strings.Join(installed, ", ") + " installed but not exporting to " + otlpURL, Fix: "terma setup"}
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
		Fix:    "terma setup",
	}
}

// RelayCheck is doctor's "agent exporting to Terma" through the local relay: its address
// is free, it delivers to env (this profile's environment), the developer's agents send to
// it, its service is this terma's, and the team is chosen and keyed.
func RelayCheck(reg *agents.Registry, relay Relay, keys Keys, projectID, env string, selected []string) Check {
	if relay.Err != nil {
		return Check{Status: Fail, Detail: relay.Err.Error()}
	}
	dir, addr, running := relay.Dir, relay.Addr, relay.Running
	if !running && relay.Squatted {
		return Check{Status: Fail, Detail: "another process is listening on " + addr + " and receives the agents' telemetry",
			Fix: "stop it, or move the relay with `terma setup --relay-addr <host:port>`"}
	}
	// A relay started without this profile's environment holds no key for its teams, and
	// drops everything it receives.
	if running && relay.Environment != "" && env != "" && relay.Environment != env {
		return Check{Status: Fail,
			Detail: "the local relay on " + addr + " delivers to the " + relay.Environment + " environment, not this profile's " + env + ", so it forwards none of this profile's sessions",
			Fix:    "terma setup"}
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
		return Check{Status: Fail, Detail: strings.Join(wrong, " and ") + " not exporting to the local relay", Fix: "terma setup"}
	}
	if !running && relay.LastFailure != "" {
		return Check{Status: Warn, Detail: "the local relay last failed to start: " + relay.LastFailure, Fix: "terma setup"}
	}
	if relay.ServiceInstalled && !relay.ServiceCurrent {
		return Check{Status: Warn,
			Detail: "the relay service was written by an earlier terma, or for another binary or environment, so the system may not start this relay",
			Fix:    "terma setup"}
	}
	if relay.ServiceInstalled && running && relay.HookStarted {
		return Check{Status: Warn,
			Detail: "a relay a hook started holds " + addr + ", so the relay service waits behind it and misses what agents export before their first hook",
			Fix:    "terma setup"}
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
		return Check{Status: Warn, Detail: "local relay on " + addr + " (" + state + "); no team is chosen, so no session is forwarded", Fix: "terma setup"}
	case !keys.has("", projectID) && !slices.ContainsFunc(reg.With[agents.RelayExporter](), func(e agents.RelayExporter) bool { return keys.has(e.Name(), projectID) }):
		return Check{Status: Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this team on this machine, so its sessions are dropped", Fix: "terma setup"}
	}
	return Check{Status: Pass, Detail: "through the local relay on " + addr + " (" + state + "); only the team's folders' sessions are forwarded"}
}

// FolderCheck says whether the team policy collects the working copy at root.
func FolderCheck(policy config.Policy, root, gitDir string, repoErr error) Check {
	if repoErr != nil {
		return Check{Status: Fail, Detail: repoErr.Error()}
	}
	var id config.Repository
	id.Names, id.Path = gitx.RepositoryFS(root, gitDir)
	names := strings.Join(id.Names, ", ")
	if id.Path != "" {
		names += ", " + id.Path
	}
	if policy.Admits(id) {
		return Check{Status: Pass, Detail: output.TildePath(root) + " is in the team's folders (" + names + ")"}
	}
	return Check{Status: Warn, Detail: "none of " + names + " is in the team's folders, so nothing here is recorded",
		Fix: "ask your team to add " + cmp.Or(id.Path, id.Names[0]) + " to its folders in Terma"}
}
