package cmd

import (
	"context"
	"fmt"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/desktoprelay"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/shim"
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
	// changes is how many files an install would still write.
	changes int
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

func judgeStatusLine(repoRoot string) statusLineVerdict {
	return classifyStatusLine((harness.Claude{}).StatusLineState(repoRoot))
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
	// routeOtherProject: connected machine-wide to a different project, with no live
	// routing to override that. Everything looks wired, and none of the spend arrives.
	routeOtherProject
	// routeLive: per-repo routing is configured and actually fires.
	routeLive
	// routeGlobal: the machine-wide config exports signals of its own.
	routeGlobal
	// routePending: routing is configured but not delivered — the shims are installed
	// and their directory is not ahead of the agent on PATH — and nothing else makes it
	// send, so sessions fall back to the machine-wide config.
	routePending
	// routeRepoDecides: connected machine-wide and exporting no signal of its own, so
	// only a repository's committed policy makes it send.
	routeRepoDecides
)

// harnessFacts is what judging one agent needs to know. gatherHarness reads it off the
// machine; a test sets it directly.
type harnessFacts struct {
	status harness.Status
	err    error
	// routed: a routing record and this project's key exist. live: that routing fires.
	routed, live bool
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
	routed := routedPerRepo(h.Name(), projectID)
	_, scoped := h.(harness.Scoped)
	f := harnessFacts{
		status:     st,
		err:        err,
		routed:     routed,
		live:       routed && perRepoLive(h.Name()),
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
	// Claude's routed --settings and Codex's -c options override repository and
	// user settings. Judge the record actually used at launch in that case.
	if f.live && shim.Routable(h.Name()) {
		rec, _, err := shim.LoadRecord(projectID)
		if err != nil {
			return "could not read per-repo telemetry settings", "terma install"
		}
		for _, signal := range rec.Signals {
			if containsSignal(harness.AllSignals, harness.Signal(signal)) {
				return "", ""
			}
		}
		return "per-repo routing has no telemetry signals enabled; sessions here send nothing", "terma install --signals traces,logs,metrics"
	}
	st := f.status
	if c, ok := h.(harness.Claude); ok {
		effective, err := c.EmissionStatus(root)
		if err != nil {
			return "could not read effective telemetry settings: " + err.Error(), "repair the Claude Code settings file named above, then restart Claude Code"
		}
		if effective.Endpoint == "" {
			return "", "" // The routing verdict already reports the missing connection.
		}
		if !effective.Connected {
			return "telemetry is disabled (CLAUDE_CODE_ENABLE_TELEMETRY); sessions here send nothing",
				"enable CLAUDE_CODE_ENABLE_TELEMETRY in Claude Code's user/repository settings, then restart Claude Code"
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
	if !f.live && len(f.status.Signals) == 0 && !f.repoAsks {
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

// judgeHarness classifies one agent. The order is the judgement: another project's
// export fails before anything else unless live routing overrides it, live routing
// outranks the machine-wide config, and routing that is configured but not live is
// only "pending" when no repository policy makes the agent send anyway.
func judgeHarness(f harnessFacts, otlpURL, projectID string) harnessVerdict {
	v := harnessVerdict{harnessFacts: f}
	st := f.status
	global := f.err == nil && st.Connected && st.Endpoint == otlpURL
	other := global && projectID != "" && st.ProjectID != "" && st.ProjectID != projectID
	silent := global && len(st.Signals) == 0
	v.sendsGlobally = global && !other && !silent
	switch {
	case other && !f.live:
		v.route, v.otherProject = routeOtherProject, st.ProjectID
	case f.live:
		v.route = routeLive
	case global && !silent:
		v.route = routeGlobal
	case f.routed && (!silent || !f.repoAsks):
		v.route = routePending
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
	for _, h := range harness.All() {
		if !h.Detect(ctx).Found {
			continue
		}
		v := judgeHarness(gatherHarness(h, projectID, root), otlpURL, projectID)
		v.name, v.displayName = h.Name(), h.DisplayName()
		out = append(out, v)
	}
	return out
}

// judgeSelectedHarnesses keeps the CLI and desktop Codex surfaces distinct for a
// developer who selected desktop during setup. A desktop-only choice must never
// be reported as a missing CLI PATH shim.
func judgeSelectedHarnesses(ctx context.Context, otlpURL, projectID, root string, selected []string) []harnessVerdict {
	selected = selectedForRepo(projectID, selected)
	verdicts := judgeHarnesses(ctx, otlpURL, projectID, root)
	if !slices.Contains(selected, codexDesktopAgent) {
		return verdicts
	}
	verdicts = slices.DeleteFunc(verdicts, func(v harnessVerdict) bool { return !slices.Contains(selected, v.name) })
	return append(verdicts, judgeDesktop(projectID))
}

func selectedForRepo(projectID string, saved []string) []string {
	selected := slices.Clone(saved)
	if projectID == "" {
		return selected
	}
	rec, ok, err := shim.LoadRecord(projectID)
	if err != nil || !ok {
		return selected
	}
	for _, choice := range []struct {
		name    string
		enabled *bool
	}{{shim.AgentCodex, rec.CLI}, {codexDesktopAgent, rec.Desktop}} {
		if choice.enabled == nil {
			continue
		}
		if *choice.enabled && !slices.Contains(selected, choice.name) {
			selected = append(selected, choice.name)
		} else if !*choice.enabled {
			selected = slices.DeleteFunc(selected, func(name string) bool { return name == choice.name })
		}
	}
	return selected
}

func judgeDesktop(projectID string) harnessVerdict {
	v := harnessVerdict{name: codexDesktopAgent, displayName: "Codex Desktop"}
	status, err := (harness.Codex{}).Status()
	route, _, routeErr := shim.LoadRecord(projectID)
	switch {
	case err != nil:
		v.emissionProblem, v.emissionFix = "could not read Codex desktop settings: "+err.Error(), "repair Codex config.toml, then run `terma install`"
	case status.Endpoint != desktoprelay.Endpoint || !slices.Contains(status.Signals, harness.SignalLogs):
		v.emissionProblem, v.emissionFix = "local logs exporter is not configured", "terma install"
	case routeErr != nil:
		v.emissionProblem, v.emissionFix = "could not read this repository's Codex desktop route: "+routeErr.Error(), "terma install"
	case !desktoprelay.ReadyForProject(projectID):
		v.emissionProblem, v.emissionFix = "this repository has no Codex desktop logs route or key", "terma install --signals logs"
	case route.IncludePrompts && !status.IncludePrompts:
		v.emissionProblem, v.emissionFix = "Codex redacts prompts before they reach the local receiver", "terma desktop connect"
	case route.IncludeToolContent && !status.IncludeToolContent:
		v.emissionProblem, v.emissionFix = "Codex suppresses tool output before it reaches the local receiver", "terma desktop connect"
	case !desktopReceiverRunning():
		v.emissionProblem, v.emissionFix = "local receiver is not running", "terma desktop connect"
	default:
		v.routed, v.live, v.route = true, true, routeLive
	}
	return v
}
