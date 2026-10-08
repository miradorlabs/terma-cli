package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
)

// `terma status` and `terma doctor` render these verdicts in their own words; one copy of
// each judgement keeps the two from disagreeing.

// CommitHooks is what terma's commit hooks do in one repository.
type CommitHooks int

// What a repository's commit hooks are doing, from the filesystem alone.
const (
	// CommitHooksOff means the team policy does not ask for them anywhere.
	CommitHooksOff CommitHooks = iota
	// CommitHooksUnadmitted means the policy does not collect this repository.
	CommitHooksUnadmitted
	// CommitHooksOwnPath means the repository sets its own core.hooksPath, which git reads
	// instead of .git/hooks, so terma's hooks there never run.
	CommitHooksOwnPath
	// CommitHooksGlobalPath means git's global config sets core.hooksPath, so git reads no
	// repository's .git/hooks.
	CommitHooksGlobalPath
	// CommitHooksLooping means a tool that installed over terma's hook runs terma's script,
	// which runs that tool's hook again: every commit fails until the next install.
	CommitHooksLooping
	// CommitHooksTaken means another tool's hook has replaced terma's and the hook terma
	// set aside is still there, so terma leaves the repository alone.
	CommitHooksTaken
	// CommitHooksNotYet means the policy asks for them and no agent session has been
	// claimed here yet, so there is nothing to install for.
	CommitHooksNotYet
	// CommitHooksInstalled means all of terma's hooks are in this repository's .git/hooks.
	CommitHooksInstalled
	// CommitHooksChained means installed and chained with another hook: one terma set aside,
	// or pre-commit's running terma's.
	CommitHooksChained
)

// JudgeCommitHooks reads what terma's commit hooks do in the repository whose git
// directory is gitDir, under the policy the hooks apply. Filesystem only: no git.
func JudgeCommitHooks(gitDir string, policy config.Policy, now time.Time) CommitHooks {
	installed, chained := repohooks.Installed(gitDir)
	wanted := policy.Validated() && !policy.Expired(now) && policy.GitHooks
	admitted := wanted && policy.Admits(config.Repository{Origin: gitx.RepositoryFS(gitDir)})
	scope := repohooks.HooksPathScope(gitDir)
	// Whether git runs .git/hooks at all comes first: a core.hooksPath set after terma's
	// hooks went in keeps them from running though their files are there.
	switch {
	case scope == "global" && admitted:
		return CommitHooksGlobalPath
	case scope != "" && admitted:
		return CommitHooksOwnPath
	case scope == "" && repohooks.Looped(gitDir):
		return CommitHooksLooping
	// Then what is there: hooks installed under a policy since switched off stay, and
	// still stamp, until `terma teardown`.
	case scope == "" && installed && chained:
		return CommitHooksChained
	case scope == "" && installed:
		return CommitHooksInstalled
	case !wanted:
		return CommitHooksOff
	case !admitted:
		return CommitHooksUnadmitted
	case repohooks.Taken(gitDir):
		return CommitHooksTaken
	}
	return CommitHooksNotYet
}

// StatusLineCapture is whether the wrapped status line feeds terma the plan's usage windows.
type StatusLineCapture int

// The status line's captures.
const (
	// StatusLineNotWrapped means terma has not wrapped the status line.
	StatusLineNotWrapped StatusLineCapture = iota
	// StatusLineUnknown means the settings could not be read.
	StatusLineUnknown
	// StatusLineOverridden means a settings file that outranks the user's defines its own.
	StatusLineOverridden
	// StatusLineBehind means capturing, with the developer's own renderer running behind it.
	StatusLineBehind
	// StatusLineDefault means capturing, with terma's default line.
	StatusLineDefault
	// StatusLineReplaced means terma wrapped it once and the developer has since replaced it.
	StatusLineReplaced
)

// StatusLineVerdict is whether the status line captures, and what stands in its way.
type StatusLineVerdict struct {
	Capture   StatusLineCapture
	Err       error
	Overrides []string
	Renderer  string
}

// StatusLineAgent is the agent whose status line terma wraps.
func StatusLineAgent(reg *agents.Registry) (agents.StatusLiner, bool) {
	s := reg.With[agents.StatusLiner]()
	if len(s) == 0 {
		return nil, false
	}
	return s[0], true
}

// JudgeStatusLine judges the status line terma wraps, as seen from repoRoot.
func JudgeStatusLine(reg *agents.Registry, repoRoot string) StatusLineVerdict {
	s, ok := StatusLineAgent(reg)
	if !ok {
		return StatusLineVerdict{}
	}
	return ClassifyStatusLine(s.StatusLineState(repoRoot))
}

// ClassifyStatusLine judges a status line from its state.
func ClassifyStatusLine(st agents.StatusLineState, err error) StatusLineVerdict {
	v := StatusLineVerdict{Err: err, Overrides: st.Overrides, Renderer: st.Renderer}
	switch {
	case err != nil:
		v.Capture = StatusLineUnknown
	case len(st.Overrides) > 0:
		v.Capture = StatusLineOverridden
	case st.Installed && st.Renderer != "":
		v.Capture = StatusLineBehind
	case st.Installed:
		v.Capture = StatusLineDefault
	case st.Replaced:
		v.Capture = StatusLineReplaced
	default:
		v.Capture = StatusLineNotWrapped
	}
	return v
}

// Route is how one agent's sessions reach this project, or why they do not.
type Route int

// The routes an agent's sessions take.
const (
	// RouteNone means not connected to this host, and nothing routes it here.
	RouteNone Route = iota
	// RouteOtherProject means connected machine-wide to a different project.
	RouteOtherProject
	// RouteGlobal means the machine-wide config exports signals of its own.
	RouteGlobal
	// RouteRepoDecides means connected machine-wide and exporting no signal of its own, so
	// only a repository's committed policy makes it send.
	RouteRepoDecides
)

// HarnessFacts is what judging one agent needs to know.
type HarnessFacts struct {
	Status harness.Status
	Err    error
	// RepoAsks means a committed repository policy switches this agent's signals on;
	// LocalScope that the agent can carry one at all.
	RepoAsks, LocalScope bool
	// EmissionProblem is a configuration that cannot emit, which routing must not hide.
	EmissionProblem, EmissionFix string
}

// HarnessVerdict is how one agent's sessions reach this project, or why they do not.
type HarnessVerdict struct {
	Name, DisplayName string
	Route             Route
	HarnessFacts
	// OtherProject is the project a RouteOtherProject agent reports to instead.
	OtherProject  string
	SendsGlobally bool
}

// Reaches reports whether the agent's sessions reach this project; bound means a repository
// the team collects, the only place its silence counts.
func (v HarnessVerdict) Reaches(bound bool) bool {
	if v.EmissionProblem != "" {
		return false
	}
	switch v.Route {
	case RouteGlobal:
		return true
	case RouteRepoDecides:
		return !bound || v.RepoAsks
	}
	return false
}

// GatherHarness reads what judging one agent needs off the machine.
func GatherHarness(reg *agents.Registry, h harness.Harness, projectID, root string) HarnessFacts {
	st, err := h.Status()
	_, scoped := h.Local(root)
	f := HarnessFacts{
		Status:     st,
		Err:        err,
		RepoAsks:   RepoAsks(h, root),
		LocalScope: scoped,
	}
	f.EmissionProblem, f.EmissionFix = emissionProblem(reg, h, root, projectID, &f)
	return f
}

func emissionProblem(reg *agents.Registry, h harness.Harness, root, projectID string, f *HarnessFacts) (string, string) {
	if root == "" {
		return "", ""
	}
	st := f.Status
	if c, ok := reg.Find[agents.EmissionChecker](h.Name()); ok {
		name := h.DisplayName()
		effective, err := c.EmissionStatus(root)
		if err != nil {
			return "could not read effective telemetry settings: " + err.Error(), "repair the " + name + " settings file named above, then restart " + name
		}
		if effective.Endpoint == "" {
			return "", "" // The routing verdict already reports the missing connection.
		}
		if !effective.Connected {
			return "telemetry is disabled (" + c.TelemetrySwitch() + "); sessions here send nothing",
				"enable " + c.TelemetrySwitch() + " in " + name + "'s user/repository settings, then restart " + name
		}
		st = effective
		f.RepoAsks = len(effective.Signals) > 0
	} else if scoped, ok := h.Local(root); ok {
		local, err := scoped.Status()
		if err != nil {
			return "could not read repository telemetry settings: " + err.Error(), "repair the repository telemetry settings file"
		}
		if local.HasPolicy {
			st = local
		}
	}
	if len(st.Signals) != 0 {
		return "", ""
	}
	// The missing-policy case is RouteRepoDecides's to explain.
	if len(f.Status.Signals) == 0 && !f.RepoAsks {
		if scoped, ok := h.Local(root); ok {
			local, err := scoped.Status()
			if err == nil && !local.HasPolicy && st.ConfigPath == f.Status.ConfigPath {
				return "", ""
			}
		} else {
			return "", ""
		}
	}
	if _, offOnly := h.(harness.LocalOffOnly); offOnly {
		return fmt.Sprintf("no OTLP telemetry signals enabled by %s; sessions here send nothing", st.ConfigPath),
			"run `terma setup` to point " + h.DisplayName() + " at the relay machine-wide (a repository cannot turn its telemetry on), then restart it"
	}
	return fmt.Sprintf("no OTLP telemetry signals enabled by %s; sessions here send nothing", st.ConfigPath),
		"review the export switches in the settings file named above (including the traces beta switch); run `terma setup` to point " + h.DisplayName() + " at the relay machine-wide, then restart it"
}

// JudgeHarness classifies one agent's machine-wide connection; another project's export
// fails before anything else.
func JudgeHarness(f HarnessFacts, otlpURL, projectID string) HarnessVerdict {
	v := HarnessVerdict{HarnessFacts: f}
	st := f.Status
	global := f.Err == nil && st.Connected && st.Endpoint == otlpURL
	other := global && projectID != "" && st.ProjectID != "" && st.ProjectID != projectID
	silent := global && len(st.Signals) == 0
	v.SendsGlobally = global && !other && !silent
	switch {
	case other:
		v.Route, v.OtherProject = RouteOtherProject, st.ProjectID
	case global && !silent:
		v.Route = RouteGlobal
	case global:
		v.Route = RouteRepoDecides
	default:
		v.Route = RouteNone
	}
	return v
}

// JudgeHarnesses judges every agent found on this machine, in registry order.
func JudgeHarnesses(ctx context.Context, reg *agents.Registry, otlpURL, projectID, root string) []HarnessVerdict {
	var out []HarnessVerdict
	for _, h := range reg.Harnesses() {
		if !h.Detect(ctx).Found {
			continue
		}
		v := JudgeHarness(GatherHarness(reg, h, projectID, root), otlpURL, projectID)
		v.Name, v.DisplayName = h.Name(), h.DisplayName()
		out = append(out, v)
	}
	return out
}

// RepoAsks reports whether the repository at root carries a committed policy that switches
// a harness's signals on.
func RepoAsks(h harness.Harness, root string) bool {
	if root == "" {
		return false
	}
	local, ok := h.Local(root)
	if !ok {
		return false
	}
	st, err := local.Status()
	return err == nil && len(st.Signals) > 0
}
