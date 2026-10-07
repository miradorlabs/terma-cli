package doctor

import (
	"cmp"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
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

// GlobalDestination says where global mode sends a folder's sessions. Only a
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

// CommitHooksCheck is doctor's wording for terma's commit hooks in this repository. Only
// a repository that should be stamped and is not warns; a policy that asks for nothing
// here, and one waiting for its first agent session, are working as intended.
func CommitHooksCheck(v CommitHooks) Check {
	switch v {
	case CommitHooksChained:
		return Check{Status: Pass, Detail: "terma's hooks in this repository's .git/hooks, chained with the hooks already there"}
	case CommitHooksInstalled:
		return Check{Status: Pass, Detail: "terma's hooks in this repository's .git/hooks"}
	case CommitHooksOwnPath:
		return Check{Status: Warn,
			Detail: "commits here are not stamped: this repository sets core.hooksPath, so git reads hooks from there and never .git/hooks",
			Fix:    "if nothing else needs it (husky and lefthook set one), unset it where `git config --show-origin core.hooksPath` says it is set (`git config --unset core.hooksPath` here, with `--worktree` if it is this worktree's own), then start an agent session here"}
	case CommitHooksGlobalPath:
		return Check{Status: Warn,
			Detail: "commits here are not stamped: your global git config sets core.hooksPath, so git reads hooks from there in every repository and never .git/hooks",
			Fix:    "if nothing else needs it, `git config --global --unset core.hooksPath`, then start an agent session here"}
	case CommitHooksLooping:
		return Check{Status: Warn,
			Detail: "commits here fail: a hook manager installed over terma's hook runs terma's script, which runs that manager's hook again (pre-commit: \"installed in migration mode\")",
			Fix:    "start an agent session here, which puts terma's hook back in front of the manager's; or reinstall the manager's hook alone (`pre-commit install -f --hook-type prepare-commit-msg`)"}
	case CommitHooksTaken:
		return Check{Status: Warn,
			Detail: "commits here may not be stamped: another tool's hook has replaced terma's in .git/hooks, and terma leaves the hook it set aside there (<hook>.pre-terma)",
			Fix:    "the .pre-terma file in .git/hooks is this repository's own earlier hook: put it back in place of the other tool's if you want it, or delete it if nothing needs it; then start an agent session here"}
	case CommitHooksUnadmitted:
		return Check{Status: Skip, Detail: "this repository is not collected, so no hook is installed here"}
	case CommitHooksNotYet:
		return Check{Status: Skip, Detail: "installed the first time an agent session is claimed here"}
	}
	return Check{Status: Skip, Detail: "your team's policy does not stamp commits"}
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
	addr, running := relay.Addr, relay.Running
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
	// Expected for a while after an update: a relay that updated itself restarts once nothing
	// it holds in memory would be lost, and one a package manager updated under steps aside
	// at the next hook. Said, so a relay kept by an agent that never pauses is not a mystery.
	if running && relay.Earlier {
		return Check{Status: Warn,
			Detail: "the local relay still runs terma " + relay.Version + ", an earlier release; it hands over to this one once it holds nothing, or at the next agent hook",
			Fix:    "terma setup"}
	}
	for _, e := range reg.With[agents.RelayExporter]() {
		if c, ok := e.(agents.RelayChecker); ok && mine(e) {
			if detail, fix, problem := c.RelayProblem(); problem {
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
	return Check{Status: Pass, Detail: "through the local relay on " + addr + " (" + state + "); it forwards the sessions your team's policy collects"}
}

// Admitting is the policy hooks apply to the working copy whose git directory is gitDir:
// the one of collected, this machine's collection, that admits it (config.Policies.Admitting),
// which may be another team's than the selected one, of this organization or another; the
// selected team's policy where none does, or outside git.
func Admitting(cfg *config.Config, collected config.Policies, gitDir string) config.Policy {
	selected := cfg.Policy.InForce(cfg.OrganizationID, cfg.AuthURL)
	if gitDir == "" {
		return selected
	}
	if p, ok := collected.Admitting(config.Repository{Origin: gitx.RepositoryFS(gitDir)}); ok {
		return p
	}
	return selected
}

// ForTeam names, on a passing repository check, the team that collects the repository
// when it is not the selected one: another team's listing, of this or another organization.
// A repository other teams of the collection list too is named as such: the first listing wins.
func ForTeam(c Check, collected config.Policies, gitDir string, pol, selected config.Policy) Check {
	if c.Status != Pass || !pol.Validated() {
		return c
	}
	if pol.TeamID != selected.TeamID {
		c.Detail += ", collected for " + pol.Label()
	}
	if gitDir == "" || pol.Global() {
		return c
	}
	id := config.Repository{Origin: gitx.RepositoryFS(gitDir)}
	var also []string
	for _, p := range collected {
		if p.TeamID != pol.TeamID && !p.Global() && p.Admits(id) {
			also = append(also, p.Label())
		}
	}
	if len(also) > 0 {
		c.Detail += "; also listed by " + strings.Join(also, " and ") + ", whose listing comes second"
	}
	return c
}

// NoRepositoriesStep is what a developer whose team lists no repositories is told.
const NoRepositoriesStep = "Ask a team admin to list repositories, or to collect every session, in the Terma web app; until then, nothing is collected."

// NoPolicyStep is what a developer whose team has no collection policy yet is told.
const NoPolicyStep = "Ask a team admin to set up your team's collection policy in the Terma web app; until then, nothing is collected."

// NothingCollectedStep is what to do about a validated policy that admits no repository.
func NothingCollectedStep(p config.Policy) string {
	if p.Unset {
		return NoPolicyStep
	}
	return NoRepositoriesStep
}

// RepositoryCheck says whether policy, the one hooks apply, collects the working copy whose
// git directory is gitDir, naming the origin terma sees.
func RepositoryCheck(policy config.Policy, gitDir string, repoErr error) Check {
	switch {
	case repoErr != nil:
		return Check{Status: Fail, Detail: repoErr.Error()}
	case !policy.Validated():
		return Check{Status: Warn, Detail: "no team collection policy on this machine, so nothing is collected", Fix: "terma setup"}
	case policy.Unset:
		return Check{Status: Warn, Detail: "your team has no collection policy", Fix: NoPolicyStep}
	case policy.AdmitsNone():
		return Check{Status: Warn, Detail: "your team lists no repositories", Fix: NoRepositoriesStep}
	case gitDir == "":
		return Check{Status: Warn, Detail: "not a git repository, so nothing here is collected"}
	}
	id := config.Repository{Origin: gitx.RepositoryFS(gitDir)}
	switch {
	case id.Origin == "":
		return Check{Status: Warn, Detail: "no origin a team could list (none, or a local path), so nothing here is collected"}
	case policy.Admits(id):
		return Check{Status: Pass, Detail: id.Origin + " is in the team's repositories"}
	}
	// Terma saves only a domain or localhost, so asking the team to list an alias is a dead end.
	if host, path, _ := strings.Cut(id.Origin, "/"); !strings.Contains(host, ".") && !strings.EqualFold(host, "localhost") {
		return Check{Status: Warn,
			Detail: "origin's host " + host + " is not a domain, most likely an SSH host alias from ~/.ssh/config; terma matches the host in the URL, so nothing here is collected",
			Fix: "point origin at the real host, the HostName for " + host + " in ~/.ssh/config, and keep your key: " +
				"git remote set-url origin 'git@<real host>:" + strings.ReplaceAll(path, "'", `'\''`) + ".git' && " +
				`git config core.sshCommand "ssh -i ~/.ssh/<your key> -o IdentitiesOnly=yes"`}
	}
	return Check{Status: Warn, Detail: id.Origin + " is not in the team's repositories, so nothing here is collected",
		Fix: "ask a team admin to add " + id.Origin + " in the Terma web app"}
}
