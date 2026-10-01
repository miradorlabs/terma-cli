package doctor

import (
	"context"
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// The verdicts `terma status` and `terma doctor` both reach. Each is judged once, here,
// and each command renders it in its own words (statusHooks / HooksCheck and
// their siblings). A status that says "connected" while doctor fails is worse than
// either, and two copies of the same judgement are how that happens.

// HookWiring is the verdict on a bound repository's commit-hook wiring.
type HookWiring struct {
	Manager hookmgr.Manager
	// Err is set when the plan could not be computed at all.
	Err error
	// Changes is how many files an install would still write, and Stale how many of
	// those are already there: written by an earlier terma, which `terma update
	// --refresh` rewrites without asking anything. A missing one is install's to add.
	Changes int
	Stale   int
	// Unpointed is the shim manager's own failure: the shims are committed, and this
	// clone's core.hooksPath (HooksPath) is not pointed at them.
	Unpointed bool
	HooksPath string
}

// JudgeHookWiring judges the commit-hook wiring of the repository at root.
func JudgeHookWiring(ctx context.Context, root string, bound *termaproject.File) HookWiring {
	det := hookmgr.Detect(root)
	if bound.Install.HookManager != "" {
		det.Manager = hookmgr.Manager(bound.Install.HookManager)
	}
	w := HookWiring{Manager: det.Manager}
	plan, err := hookmgr.PlanInstall(root, det)
	w.Err, w.Changes = err, len(plan.Changes)
	for _, c := range plan.Changes {
		if c.Before != nil {
			w.Stale++
		}
	}
	if det.Manager == hookmgr.GitShim {
		w.HooksPath = gitx.ConfigGet(ctx, root, "core.hooksPath")
		w.Unpointed = w.HooksPath != hookmgr.ShimDir
	}
	return w
}

// StatusLineCapture is whether Claude Code's status line feeds terma the plan's usage
// windows, and why not when it does not.
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

// Route is how one agent's sessions, started here and now, reach this project —
// or why they do not.
type Route int

// The routes an agent's sessions take.
const (
	// RouteNone means not connected to this host, and nothing routes it here.
	RouteNone Route = iota
	// RouteOtherProject means connected machine-wide to a different project. Everything
	// looks wired, and none of the spend arrives.
	RouteOtherProject
	// RouteGlobal means the machine-wide config exports signals of its own.
	RouteGlobal
	// RouteHooks means the agent reports through the repository's hooks and the spool (Codex
	// Desktop's route, before the local relay carried its own export).
	RouteHooks
	// RouteRepoDecides means connected machine-wide and exporting no signal of its own, so
	// only a repository's committed policy makes it send.
	RouteRepoDecides
)

// HarnessFacts is what judging one agent needs to know. GatherHarness reads it off the
// machine; a test sets it directly.
type HarnessFacts struct {
	Status harness.Status
	Err    error
	// RepoAsks: the repository the CLI stands in carries a committed policy that
	// switches this agent's signals on. LocalScope: the agent can carry one at all.
	RepoAsks, LocalScope bool
	// EmissionProblem prevents routing or another healthy agent from hiding a
	// configuration that cannot emit telemetry. These checks never launch an agent.
	EmissionProblem, EmissionFix string
}

// HarnessVerdict is how one agent's sessions reach this project, or why they do not.
type HarnessVerdict struct {
	Name, DisplayName string
	Route             Route
	HarnessFacts
	// OtherProject is the project a RouteOtherProject agent reports to instead.
	OtherProject string
	// SendsGlobally: the machine-wide config alone would deliver to this project.
	SendsGlobally bool
}

// Reaches reports whether the agent's sessions reach this project. bound says the CLI
// stands in an installed repository, the only place a repository's silence counts.
func (v HarnessVerdict) Reaches(bound bool) bool {
	if v.EmissionProblem != "" {
		return false
	}
	switch v.Route {
	case RouteGlobal, RouteHooks:
		return true
	case RouteRepoDecides:
		return !bound || v.RepoAsks
	}
	return false
}

// GatherHarness reads what judging one agent needs off the machine.
func GatherHarness(reg *agents.Registry, h harness.Harness, projectID, root string) HarnessFacts {
	st, err := h.Status()
	_, scoped := h.(harness.Scoped)
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
	} else if scoped, ok := h.(harness.Scoped); ok {
		local, err := scoped.Local(root).Status()
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
	// Leave the missing-policy case to RouteRepoDecides, which explains the
	// machine-wide 'repos decide' arrangement and its plain-install fix.
	if len(f.Status.Signals) == 0 && !f.RepoAsks {
		if scoped, ok := h.(harness.Scoped); ok {
			local, err := scoped.Local(root).Status()
			if err == nil && !local.HasPolicy && st.ConfigPath == f.Status.ConfigPath {
				return "", ""
			}
		} else {
			return "", ""
		}
	}
	return fmt.Sprintf("no OTLP telemetry signals enabled by %s; sessions here send nothing", st.ConfigPath),
		"review the export switches in the settings file named above (including the traces beta switch); run `terma install --signals traces,logs,metrics` to enable repository telemetry, then restart " + h.DisplayName()
}

// JudgeHarness classifies one agent's machine-wide connection (one made before the local
// relay, which judges itself: RelayCheck). The order is the judgement: another
// project's export fails before anything else.
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

// JudgeHarnesses judges every agent found on this machine, in registry order. root is
// empty outside a repository, where no repository policy can be asking.
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

// JudgeSelectedHarnesses keeps an agent's surfaces distinct for a developer who
// selected one with a check of its own (Codex Desktop): a desktop-only choice must never
// be reported as a missing CLI.
func JudgeSelectedHarnesses(ctx context.Context, reg *agents.Registry, otlpURL, projectID, root string, saved []string) []HarnessVerdict {
	selected := SelectedForRepo(reg, projectID, saved)
	verdicts := JudgeHarnesses(ctx, reg, otlpURL, projectID, root)
	var checked []HarnessVerdict
	for _, name := range selected {
		if v, ok := JudgeSurface(reg, name, root, projectID); ok {
			checked = append(checked, v)
		}
	}
	if len(checked) == 0 {
		return verdicts
	}
	verdicts = slices.DeleteFunc(verdicts, func(v HarnessVerdict) bool { return !slices.Contains(selected, v.Name) })
	return append(verdicts, checked...)
}

// SelectedForRepo is the saved selection as this repository's routing record narrows it:
// for an agent run as more than one surface, the surfaces the record routes here. A
// record that names no surface (one an earlier build wrote) leaves the selection alone,
// so a surface's own check still says what is missing.
func SelectedForRepo(reg *agents.Registry, projectID string, saved []string) []string {
	selected := slices.Clone(saved)
	if projectID == "" {
		return selected
	}
	rec, ok, err := routing.LoadRecord(projectID)
	if err != nil || !ok || len(rec.Surfaces) == 0 {
		return selected
	}
	for _, a := range reg.With[agents.Surfaced]() {
		for _, s := range a.Surfaces() {
			routed := slices.Contains(rec.Surfaces, s.Name)
			if routed && !slices.Contains(selected, s.Name) {
				selected = append(selected, s.Name)
			} else if !routed {
				selected = slices.DeleteFunc(selected, func(name string) bool { return name == s.Name })
			}
		}
	}
	return selected
}

// JudgeSurface is the verdict of a surface with a check of its own.
func JudgeSurface(reg *agents.Registry, surface, root, projectID string) (HarnessVerdict, bool) {
	st, ok, err := reg.CheckSurface(surface, root, projectID)
	if !ok {
		return HarnessVerdict{}, false
	}
	s, _, _ := reg.Surface(surface)
	v := HarnessVerdict{Name: surface, DisplayName: s.DisplayName}
	switch {
	case err != nil:
		v.EmissionProblem, v.EmissionFix = "could not check "+s.DisplayName+": "+err.Error(), "terma install"
	case !st.Ready:
		v.EmissionProblem, v.EmissionFix = st.Problem, st.Fix
	default:
		v.Route = RouteHooks
	}
	return v, true
}

// RepoAsks reports whether the repository at root carries a committed policy that switches
// a harness's signals on. It is the other half of a machine-wide connect made with
// `--exports repos`: that connect holds the endpoint and the key and exports nothing, so
// whether a session here sends anything is this file's decision.
func RepoAsks(h harness.Harness, root string) bool {
	scoped, ok := h.(harness.Scoped)
	if !ok || root == "" {
		return false
	}
	st, err := scoped.Local(root).Status()
	return err == nil && len(st.Signals) > 0
}
