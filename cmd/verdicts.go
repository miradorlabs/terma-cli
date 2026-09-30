package cmd

import (
	"context"
	"fmt"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// The verdicts `terma status` and `terma doctor` both reach. Each is judged once, here,
// and each command renders it in its own words (statusHooks / doctorHooksCheck and
// their siblings). A status that says "connected" while doctor fails is worse than
// either, and two copies of the same judgement are how that happens.

// hookWiring is the verdict on a bound repository's commit-hook wiring.
type hookWiring struct {
	manager hookmgr.Manager
	// err is set when the plan could not be computed at all.
	err error
	// changes is how many files an install would still write, and stale how many of
	// those are already there: written by an earlier terma, which `terma update
	// --refresh` rewrites without asking anything. A missing one is install's to add.
	changes int
	stale   int
	// unpointed is the shim manager's own failure: the shims are committed, and this
	// clone's core.hooksPath (hooksPath) is not pointed at them.
	unpointed bool
	hooksPath string
}

func judgeHookWiring(ctx context.Context, root string, bound *termaproject.File) hookWiring {
	det := hookmgr.Detect(root)
	if bound.Install.HookManager != "" {
		det.Manager = hookmgr.Manager(bound.Install.HookManager)
	}
	w := hookWiring{manager: det.Manager}
	plan, err := hookmgr.PlanInstall(root, det)
	w.err, w.changes = err, len(plan.Changes)
	for _, c := range plan.Changes {
		if c.Before != nil {
			w.stale++
		}
	}
	if det.Manager == hookmgr.GitShim {
		w.hooksPath = gitx.ConfigGet(ctx, root, "core.hooksPath")
		w.unpointed = w.hooksPath != hookmgr.ShimDir
	}
	return w
}

// statusLineCapture is whether Claude Code's status line feeds terma the plan's usage
// windows, and why not when it does not.
type statusLineCapture int

const (
	statusLineNotWrapped statusLineCapture = iota
	// statusLineUnknown: the settings could not be read.
	statusLineUnknown
	// statusLineOverridden: a settings file that outranks the user's defines its own.
	statusLineOverridden
	// statusLineBehind: capturing, with the developer's own renderer running behind it.
	statusLineBehind
	// statusLineDefault: capturing, with terma's default line.
	statusLineDefault
	// statusLineReplaced: terma wrapped it once and the developer has since replaced it.
	statusLineReplaced
)

type statusLineVerdict struct {
	capture   statusLineCapture
	err       error
	overrides []string
	renderer  string
}

// statusLineAgent is the agent whose status line terma wraps, and its harness.
func statusLineAgent() (agents.Exporting, harness.StatusLiner, bool) {
	for _, e := range registered.With[agents.Exporting]() {
		if s, ok := e.Harness().(harness.StatusLiner); ok {
			return e, s, true
		}
	}
	return nil, nil, false
}

func judgeStatusLine(repoRoot string) statusLineVerdict {
	_, s, ok := statusLineAgent()
	if !ok {
		return statusLineVerdict{}
	}
	return classifyStatusLine(s.StatusLineState(repoRoot))
}

func classifyStatusLine(st harness.StatusLineState, err error) statusLineVerdict {
	v := statusLineVerdict{err: err, overrides: st.Overrides, renderer: st.Renderer}
	switch {
	case err != nil:
		v.capture = statusLineUnknown
	case len(st.Overrides) > 0:
		v.capture = statusLineOverridden
	case st.Installed && st.Renderer != "":
		v.capture = statusLineBehind
	case st.Installed:
		v.capture = statusLineDefault
	case st.Replaced:
		v.capture = statusLineReplaced
	default:
		v.capture = statusLineNotWrapped
	}
	return v
}

// harnessRoute is how one agent's sessions, started here and now, reach this project —
// or why they do not.
type harnessRoute int

const (
	// routeNone: not connected to this host, and nothing routes it here.
	routeNone harnessRoute = iota
	// routeOtherProject: connected machine-wide to a different project. Everything
	// looks wired, and none of the spend arrives.
	routeOtherProject
	// routeGlobal: the machine-wide config exports signals of its own.
	routeGlobal
	// routeHooks: the agent reports through the repository's hooks and the spool (Codex
	// Desktop's route, before the local relay carried its own export).
	routeHooks
	// routeRepoDecides: connected machine-wide and exporting no signal of its own, so
	// only a repository's committed policy makes it send.
	routeRepoDecides
)

// harnessFacts is what judging one agent needs to know. gatherHarness reads it off the
// machine; a test sets it directly.
type harnessFacts struct {
	status harness.Status
	err    error
	// repoAsks: the repository the CLI stands in carries a committed policy that
	// switches this agent's signals on. localScope: the agent can carry one at all.
	repoAsks, localScope bool
	// emissionProblem prevents routing or another healthy agent from hiding a
	// configuration that cannot emit telemetry. These checks never launch an agent.
	emissionProblem, emissionFix string
}

type harnessVerdict struct {
	name, displayName string
	route             harnessRoute
	harnessFacts
	// otherProject is the project a routeOtherProject agent reports to instead.
	otherProject string
	// sendsGlobally: the machine-wide config alone would deliver to this project.
	sendsGlobally bool
}

func gatherHarness(h harness.Harness, projectID, root string) harnessFacts {
	st, err := h.Status()
	_, scoped := h.(harness.Scoped)
	f := harnessFacts{
		status:     st,
		err:        err,
		repoAsks:   repoAsks(h, root),
		localScope: scoped,
	}
	f.emissionProblem, f.emissionFix = emissionProblem(h, root, projectID, &f)
	return f
}

func emissionProblem(h harness.Harness, root, projectID string, f *harnessFacts) (string, string) {
	if root == "" {
		return "", ""
	}
	st := f.status
	if c, ok := h.(harness.EmissionChecker); ok {
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
		f.repoAsks = len(effective.Signals) > 0
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
	// Leave the missing-policy case to routeRepoDecides, which explains the
	// machine-wide 'repos decide' arrangement and its plain-install fix.
	if len(f.status.Signals) == 0 && !f.repoAsks {
		if scoped, ok := h.(harness.Scoped); ok {
			local, err := scoped.Local(root).Status()
			if err == nil && !local.HasPolicy && st.ConfigPath == f.status.ConfigPath {
				return "", ""
			}
		} else {
			return "", ""
		}
	}
	return fmt.Sprintf("no OTLP telemetry signals enabled by %s; sessions here send nothing", st.ConfigPath),
		"review the export switches in the settings file named above (including the traces beta switch); run `terma install --signals traces,logs,metrics` to enable repository telemetry, then restart " + h.DisplayName()
}

// judgeHarness classifies one agent's machine-wide connection (one made before the local
// relay, which judges itself: relayDoctorCheck). The order is the judgement: another
// project's export fails before anything else.
func judgeHarness(f harnessFacts, otlpURL, projectID string) harnessVerdict {
	v := harnessVerdict{harnessFacts: f}
	st := f.status
	global := f.err == nil && st.Connected && st.Endpoint == otlpURL
	other := global && projectID != "" && st.ProjectID != "" && st.ProjectID != projectID
	silent := global && len(st.Signals) == 0
	v.sendsGlobally = global && !other && !silent
	switch {
	case other:
		v.route, v.otherProject = routeOtherProject, st.ProjectID
	case global && !silent:
		v.route = routeGlobal
	case global:
		v.route = routeRepoDecides
	default:
		v.route = routeNone
	}
	return v
}

// judgeHarnesses judges every agent found on this machine, in registry order. root is
// empty outside a repository, where no repository policy can be asking.
func judgeHarnesses(ctx context.Context, otlpURL, projectID, root string) []harnessVerdict {
	var out []harnessVerdict
	for _, h := range registered.Harnesses() {
		if !h.Detect(ctx).Found {
			continue
		}
		v := judgeHarness(gatherHarness(h, projectID, root), otlpURL, projectID)
		v.name, v.displayName = h.Name(), h.DisplayName()
		out = append(out, v)
	}
	return out
}

// judgeSelectedHarnesses keeps an agent's surfaces distinct for a developer who
// selected one with a check of its own (Codex Desktop): a desktop-only choice must never
// be reported as a missing CLI.
func judgeSelectedHarnesses(ctx context.Context, otlpURL, projectID, root string, saved []string) []harnessVerdict {
	selected := selectedForRepo(projectID, saved)
	verdicts := judgeHarnesses(ctx, otlpURL, projectID, root)
	var checked []harnessVerdict
	for _, name := range selected {
		if v, ok := judgeSurface(name, root, projectID); ok {
			checked = append(checked, v)
		}
	}
	if len(checked) == 0 {
		return verdicts
	}
	verdicts = slices.DeleteFunc(verdicts, func(v harnessVerdict) bool { return !slices.Contains(selected, v.name) })
	return append(verdicts, checked...)
}

// selectedForRepo is the saved selection as this repository's routing record narrows it:
// for an agent run as more than one surface, the surfaces the record routes here. A
// record that names no surface (one an earlier build wrote) leaves the selection alone,
// so a surface's own check still says what is missing.
func selectedForRepo(projectID string, saved []string) []string {
	selected := slices.Clone(saved)
	if projectID == "" {
		return selected
	}
	rec, ok, err := routing.LoadRecord(projectID)
	if err != nil || !ok || len(rec.Surfaces) == 0 {
		return selected
	}
	for _, a := range registered.With[agents.Surfaced]() {
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

// judgeSurface is the verdict of a surface with a check of its own.
func judgeSurface(surface, root, projectID string) (harnessVerdict, bool) {
	st, ok, err := registered.CheckSurface(surface, root, projectID)
	if !ok {
		return harnessVerdict{}, false
	}
	s, _, _ := registered.Surface(surface)
	v := harnessVerdict{name: surface, displayName: s.DisplayName}
	switch {
	case err != nil:
		v.emissionProblem, v.emissionFix = "could not check "+s.DisplayName+": "+err.Error(), "terma install"
	case !st.Ready:
		v.emissionProblem, v.emissionFix = st.Problem, st.Fix
	default:
		v.route = routeHooks
	}
	return v, true
}
