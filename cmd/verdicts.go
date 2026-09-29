package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
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
	// routeNone: not pointed at terma's relay or at Terma.
	routeNone harnessRoute = iota
	// routeOtherProject: exporting straight to Terma with another project's key (the
	// machine project, in direct mode). Everything looks wired, and none of the spend
	// arrives here.
	routeOtherProject
	// routeRelay: pointed at terma's relay, which sends each session to its
	// repository's project.
	routeRelay
	// routeGlobal: exporting straight to Terma, to this project.
	routeGlobal
	// routeLive: routed per repository by the agent itself (OpenCode's plugin).
	routeLive
	// routeRepoDecides: connected machine-wide and exporting no signal of its own, so
	// only a repository's committed policy makes it send.
	routeRepoDecides
)

// harnessFacts is what judging one agent needs to know. gatherHarness reads it off the
// machine; a test sets it directly.
type harnessFacts struct {
	status harness.Status
	err    error
	// routed: the agent routes itself per repository (OpenCode's plugin with this
	// project's key).
	routed bool
	// repoAsks: the repository the CLI stands in carries a committed policy that
	// switches this agent's signals on. localScope: the agent can carry one at all.
	repoAsks, localScope bool
	// emissionProblem prevents a healthy-looking agent from hiding a configuration that
	// cannot emit telemetry. These checks never launch an agent.
	emissionProblem, emissionFix string
}

type harnessVerdict struct {
	name, displayName string
	route             harnessRoute
	harnessFacts
	// otherProject is the project a routeOtherProject agent reports to instead.
	otherProject string
}

func gatherHarness(h harness.Harness, projectID, root string) harnessFacts {
	st, err := h.Status()
	_, scoped := h.(harness.Scoped)
	f := harnessFacts{
		status:     st,
		err:        err,
		routed:     routedPerRepo(h.Name(), projectID),
		repoAsks:   repoAsks(h, root),
		localScope: scoped,
	}
	f.emissionProblem, f.emissionFix = emissionProblem(h, root, &f)
	return f
}

// routedPerRepo reports whether an agent routes itself to projectID from here: OpenCode's
// per-repository plugin, with this project's key on the machine.
func routedPerRepo(name, projectID string) bool {
	if name != "opencode" || projectID == "" || keystore.GetFor("opencode", projectID) == "" {
		return false
	}
	st, err := (harness.OpenCode{}).Status()
	return err == nil && st.Exists
}

// repoAsks reports whether the repository at root carries a committed policy that switches
// a harness's signals on.
func repoAsks(h harness.Harness, root string) bool {
	scoped, ok := h.(harness.Scoped)
	if !ok || root == "" {
		return false
	}
	st, err := scoped.Local(root).Status()
	return err == nil && len(st.Signals) > 0
}

func emissionProblem(h harness.Harness, root string, f *harnessFacts) (string, string) {
	if root == "" {
		return "", ""
	}
	st := f.status
	if c, ok := h.(harness.Claude); ok {
		effective, err := c.EmissionStatus(root)
		if err != nil {
			return "could not read effective telemetry settings: " + err.Error(), "repair the Claude Code settings file named above, then restart Claude Code"
		}
		if effective.Endpoint == "" {
			return "", "" // The route verdict already reports the missing connection.
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
	// machine-wide 'repos decide' arrangement and its fix.
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
		"review the export switches in the settings file named above (including the traces beta switch); `terma setup --signals traces,logs,metrics` enables them machine-wide, then restart " + h.DisplayName()
}

// judgeHarness classifies one agent. relayEndpoint is where terma's relay listens, "" when
// this machine has none. The relay routes a session by its repository, so an agent
// pointed at it reports here whatever the machine project is; one exporting straight to
// Terma reports to its key's project.
func judgeHarness(f harnessFacts, otlpURL, relayEndpoint, projectID string) harnessVerdict {
	v := harnessVerdict{harnessFacts: f}
	st := f.status
	endpoint := strings.TrimRight(st.Endpoint, "/")
	connected := f.err == nil && st.Connected
	viaRelay := connected && relayEndpoint != "" && endpoint == relayEndpoint
	direct := connected && endpoint == strings.TrimRight(otlpURL, "/")
	silent := len(st.Signals) == 0
	switch {
	case f.routed:
		v.route = routeLive
	case direct && projectID != "" && st.ProjectID != "" && st.ProjectID != projectID:
		v.route, v.otherProject = routeOtherProject, st.ProjectID
	case (viaRelay || direct) && silent:
		v.route = routeRepoDecides
	case viaRelay:
		v.route = routeRelay
	case direct:
		v.route = routeGlobal
	default:
		v.route = routeNone
	}
	return v
}

// relayEndpointHere is where this machine's relay listens, "" when setup has not
// configured one.
func relayEndpointHere() string {
	rc, err := relayLoad()
	if err != nil {
		return ""
	}
	return rc.Endpoint()
}

// judgeHarnesses judges every agent found on this machine, in registry order. root is
// empty outside a repository, where no repository policy can be asking.
func judgeHarnesses(ctx context.Context, otlpURL, projectID, root string) []harnessVerdict {
	relayEndpoint := relayEndpointHere()
	var out []harnessVerdict
	for _, h := range harness.All() {
		if !h.Detect(ctx).Found {
			continue
		}
		v := judgeHarness(gatherHarness(h, projectID, root), otlpURL, relayEndpoint, projectID)
		v.name, v.displayName = h.Name(), h.DisplayName()
		out = append(out, v)
	}
	return out
}

// judgeSelectedHarnesses keeps the CLI and desktop Codex surfaces distinct for a
// developer who selected desktop during setup. Codex Desktop reads Codex's own global
// configuration, so it is judged by it, under its own name — and judged even when no
// Codex CLI is on PATH to be found.
func judgeSelectedHarnesses(ctx context.Context, otlpURL, projectID, root string, selected []string) []harnessVerdict {
	verdicts := judgeHarnesses(ctx, otlpURL, projectID, root)
	if !slices.Contains(selected, codexDesktopAgent) {
		return verdicts
	}
	verdicts = slices.DeleteFunc(verdicts, func(v harnessVerdict) bool { return !slices.Contains(selected, v.name) })
	h := harness.Codex{}
	v := judgeHarness(gatherHarness(h, projectID, root), otlpURL, relayEndpointHere(), projectID)
	v.name, v.displayName = codexDesktopAgent, "Codex Desktop"
	return append(verdicts, v)
}
