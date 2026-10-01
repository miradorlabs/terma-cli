package doctor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// AgentHooksCheck reports, for the agents wired in this repository, whether their hooks
// are in place and whether the agent will run them. It is shared by doctor and status so
// the two cannot disagree about it.
//
// The repository's hooks files are the record of what is wired (adapter.Wired). An agent
// they wire is checked whoever uses it: wiring an older terma wrote is stale for everyone.
// An agent they do not wire is missing only when this developer named it among their
// agents (mine) — a repository nobody opens in Cursor is not missing anything, and a
// developer who has not said which agents they use is not told about every agent terma
// knows. Trust is different: some agents refuse to run a committed hook until the
// developer has trusted it, or the repository, once from inside the agent — the wiring
// looks perfect and nothing runs, a silence worth naming — but only for an agent this
// developer uses (mine; empty means "has not said", so all of them). A colleague's agent
// is naturally untrusted here and costs this developer nothing.
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
		// Codex Desktop is wired through the Codex hooks file.
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
			// Wired, so the file is there: an earlier terma wrote it, and a refresh
			// rewrites it without re-asking everything install asks.
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
			// Unreadable is not untrusted: say so, and do not count it against anyone.
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

// stateCheck reports saved state this build has not finished migrating, and has nothing
// to say (false) when every migration it has is applied.
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

// HooksCheck is doctor's wording for the commit-hook verdict. An unreadable plan
// fails here; status does not look at why a plan could not be computed.
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

// HarnessCheck folds every agent's verdict into doctor's one export check. bound
// says the CLI stands in an installed repository, the only place "this repository does
// not route it" means anything.
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
			return Check{Status: Fail, Detail: v.DisplayName + " reports to project " + v.OtherProject + ", not " + projectID, Fix: "terma install"}
		case RouteHooks:
			connected = append(connected, v.DisplayName+" (repository hooks)")
		case RouteGlobal:
			connected = append(connected, v.DisplayName)
		case RouteRepoDecides:
			connected = append(connected, v.DisplayName)
			repoDecides = append(repoDecides, v.DisplayName)
			// An agent that cannot carry a repository policy at all (Codex) cannot be
			// asked for by one either, so it is as silent here as one whose repository
			// simply has none. doctor used to skip it and report "this repository asks"
			// for a config status said sent nothing.
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
	// A globally-connected-but-silent harness that this repository neither routes nor
	// carries a committed policy for is the one case worth a word — and only in the
	// repository the developer is standing in.
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
		Detail: detail + "; this repository does not route " + strings.Join(silent, ", ") + " to its project, so its sessions send nothing",
		Fix:    "terma install",
	}
}

// RelayCheck is doctor's "agent exporting to Terma" on a machine that exports
// through the local relay: the relay can run (or runs) on its address with no one else
// there, each of the developer's agents sends to it, and this repository's sessions
// can leave — it is bound and this machine holds its project's key.
func RelayCheck(reg *agents.Registry, projectID string, selected []string) Check {
	dir, err := claim.Dir()
	if err != nil {
		return Check{Status: Fail, Detail: err.Error()}
	}
	addr := daemon.Addr(dir)
	running := daemon.Running(dir)
	if !running && daemon.Squatted(addr) {
		return Check{Status: Fail, Detail: "another process is listening on " + addr + " and receives the agents' telemetry",
			Fix: "stop it, or move the relay with `terma relay setup --addr`"}
	}
	// The developer's selected, or the supported ones when none are recorded.
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
	case keystore.Get(projectID) == "" && !slices.ContainsFunc(reg.With[agents.RelayExporter](), func(e agents.RelayExporter) bool { return keystore.GetFor(e.Name(), projectID) != "" }):
		return Check{Status: Warn, Detail: "local relay on " + addr + " (" + state + "); no key for this project on this machine, so its sessions are dropped", Fix: "terma install"}
	}
	return Check{Status: Pass, Detail: "through the local relay on " + addr + " (" + state + "); only this repository's sessions are forwarded"}
}
