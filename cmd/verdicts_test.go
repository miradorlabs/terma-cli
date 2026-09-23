package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
)

// Each test below hands ONE verdict to both commands' renderers. status and doctor
// used to reach these judgements separately, which is how one could say "connected"
// while the other failed; now a disagreement has to be written down here to exist.

func TestHookWiringVerdictInBothCommands(t *testing.T) {
	cases := []struct {
		name         string
		w            hookWiring
		doctorStatus doctor.Status
		doctorDetail string
		statusState  string
		statusWired  bool
	}{
		{
			name:         "wired through a hook manager",
			w:            hookWiring{manager: hookmgr.Husky},
			doctorStatus: doctor.Pass, doctorDetail: string(hookmgr.Husky),
			statusState: "wired", statusWired: true,
		},
		{
			name:         "wired through terma's shims",
			w:            hookWiring{manager: hookmgr.GitShim, hooksPath: hookmgr.ShimDir},
			doctorStatus: doctor.Pass, doctorDetail: string(hookmgr.GitShim) + " shims, core.hooksPath set",
			statusState: "wired", statusWired: true,
		},
		{
			name:         "an install would still write files",
			w:            hookWiring{manager: hookmgr.Lefthook, changes: 2},
			doctorStatus: doctor.Fail, doctorDetail: string(hookmgr.Lefthook) + " wiring is missing or stale (2 file change(s))",
			statusState: "NOT wired (run `terma install`)", statusWired: false,
		},
		{
			name:         "shims committed, this clone not pointed at them",
			w:            hookWiring{manager: hookmgr.GitShim, unpointed: true},
			doctorStatus: doctor.Fail, doctorDetail: "shims are committed but git is not pointed at them in this clone (core.hooksPath=unset)",
			statusState: "NOT wired (run `terma install`)", statusWired: false,
		},
		{
			// Where the two commands used to disagree: doctor failed a plan it could not
			// compute, and status read it as nothing left to write and credited commit
			// stamping. Neither calls it wired now, and both say why.
			name:         "the plan cannot be computed",
			w:            hookWiring{manager: hookmgr.Husky, err: errors.New("read .husky/pre-commit: permission denied")},
			doctorStatus: doctor.Fail, doctorDetail: "read .husky/pre-commit: permission denied",
			statusState: "could not be checked — read .husky/pre-commit: permission denied", statusWired: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			check := doctorHooksCheck(tc.w)
			if check.Status != tc.doctorStatus || check.Detail != tc.doctorDetail {
				t.Errorf("doctor = %v %q; want %v %q", check.Status, check.Detail, tc.doctorStatus, tc.doctorDetail)
			}
			if check.Status == doctor.Fail && check.Fix != "terma install" {
				t.Errorf("a failed wiring check should name the fix, got %q", check.Fix)
			}
			state, wired := statusHooks(tc.w)
			if state != tc.statusState || wired != tc.statusWired {
				t.Errorf("status = %q, %v; want %q, %v", state, wired, tc.statusState, tc.statusWired)
			}
		})
	}
}

func TestStatusLineVerdictInBothCommands(t *testing.T) {
	cases := []struct {
		name         string
		st           harness.StatusLineState
		err          error
		capture      statusLineCapture
		doctorStatus doctor.Status
		doctorDetail string
		status       string
	}{
		{
			name: "settings unreadable", err: errors.New("permission denied"),
			capture:      statusLineUnknown,
			doctorStatus: doctor.Warn, doctorDetail: "permission denied",
			status: "unknown (permission denied)",
		},
		{
			// An override outranks everything else about the file, installed or not.
			name:         "overridden by a repository's own settings",
			st:           harness.StatusLineState{Installed: true, Overrides: []string{".claude/settings.json"}},
			capture:      statusLineOverridden,
			doctorStatus: doctor.Warn, doctorDetail: "overridden by .claude/settings.json; plan usage is not captured in this repository",
			status: "overridden here by .claude/settings.json — plan usage is not captured in this repository",
		},
		{
			name:         "capturing, the developer's renderer behind it",
			st:           harness.StatusLineState{Installed: true, Renderer: "~/bin/line.sh"},
			capture:      statusLineBehind,
			doctorStatus: doctor.Pass, doctorDetail: "capturing plan usage; ~/bin/line.sh runs behind it",
			status: "capturing plan usage; your own (~/bin/line.sh) runs behind it",
		},
		{
			name:         "capturing with terma's default line",
			st:           harness.StatusLineState{Installed: true},
			capture:      statusLineDefault,
			doctorStatus: doctor.Pass, doctorDetail: "capturing plan usage (terma's default line)",
			status: "capturing plan usage (terma's default line)",
		},
		{
			name:         "replaced since terma wrapped it",
			st:           harness.StatusLineState{Replaced: true},
			capture:      statusLineReplaced,
			doctorStatus: doctor.Warn, doctorDetail: "replaced by your own status line since terma wrapped it; plan usage is not captured",
			status: "replaced by your own since terma wrapped it — plan usage is NOT captured (run `terma install`)",
		},
		{
			name:         "never wrapped",
			capture:      statusLineNotWrapped,
			doctorStatus: doctor.Warn, doctorDetail: "not wrapped; plan usage is not captured",
			status: "not wrapped — plan usage is NOT captured (run `terma install`)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyStatusLine(tc.st, tc.err)
			if v.capture != tc.capture {
				t.Fatalf("capture = %v, want %v", v.capture, tc.capture)
			}
			check := doctorStatusLineCheck(v)
			if check.Status != tc.doctorStatus || check.Detail != tc.doctorDetail {
				t.Errorf("doctor = %v %q; want %v %q", check.Status, check.Detail, tc.doctorStatus, tc.doctorDetail)
			}
			if got := statusLineSummary(v); got != tc.status {
				t.Errorf("status = %q; want %q", got, tc.status)
			}
			// Capturing is the only state doctor passes, and the only one status does
			// not flag: the two agree on which states work.
			capturing := v.capture == statusLineBehind || v.capture == statusLineDefault
			if (check.Status == doctor.Pass) != capturing || strings.HasPrefix(tc.status, "capturing") != capturing {
				t.Errorf("the commands disagree about whether %v captures", v.capture)
			}
		})
	}
}

func TestHarnessVerdictInBothCommands(t *testing.T) {
	// The pending case asks where the shims should go, which reads the shell's
	// startup file; keep that away from the developer's own.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")

	const otlp = "https://otel-dev.mirador.org"
	const project = "6796a71f-7949-40f1-bde8-b87a74071686"
	const elsewhere = "c970664b-ba35-4cdd-b7a9-d5acadb327f6"
	sending := harness.Status{Connected: true, Endpoint: otlp, ProjectID: project, Signals: harness.AllSignals}
	silent := harness.Status{Connected: true, Endpoint: otlp, ProjectID: project}
	other := harness.Status{Connected: true, Endpoint: otlp, ProjectID: elsewhere, Signals: harness.AllSignals}

	cases := []struct {
		name         string
		facts        harnessFacts
		bound        bool
		route        harnessRoute
		doctorStatus doctor.Status
		doctorDetail string
		status       string
		statusOK     bool
	}{
		{
			name:  "machine-wide config sends to this project",
			facts: harnessFacts{status: sending}, bound: true,
			route:        routeGlobal,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code → " + otlp,
			status: "→ connected", statusOK: true,
		},
		{
			name:  "per-repo routing fires",
			facts: harnessFacts{routed: true, live: true}, bound: true,
			route:        routeLive,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code (per-repo) → " + otlp,
			status: "→ connected (per-repo routing)", statusOK: true,
		},
		{
			// Same verdict, each command's own emphasis: doctor names the routing,
			// status says the plainer thing when the machine-wide config delivers too.
			name:  "per-repo routing fires over a config that already sends here",
			facts: harnessFacts{status: sending, routed: true, live: true}, bound: true,
			route:        routeLive,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code (per-repo) → " + otlp,
			status: "→ connected", statusOK: true,
		},
		{
			name:  "live routing overrides a config that reports elsewhere",
			facts: harnessFacts{status: other, routed: true, live: true}, bound: true,
			route:        routeLive,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code (per-repo) → " + otlp,
			status: "→ connected (per-repo routing)", statusOK: true,
		},
		{
			name:  "reports to another project",
			facts: harnessFacts{status: other}, bound: true,
			route:        routeOtherProject,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code reports to project " + elsewhere + ", not " + project,
			status: "→ reporting to project " + elsewhere + ", not this one — run `terma install`", statusOK: false,
		},
		{
			// Neither command calls this working. doctor names the project it goes to;
			// status names the routing that would fix it and is not live yet.
			name:  "reports to another project, routing configured but not live",
			facts: harnessFacts{status: other, routed: true}, bound: true,
			route:        routeOtherProject,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code reports to project " + elsewhere + ", not " + project,
			status: "→ per-repo routing configured, but terma's shim is not ahead of it on your PATH", statusOK: false,
		},
		{
			name:  "routing configured but not live, nothing else sends",
			facts: harnessFacts{routed: true}, bound: true,
			route:        routePending,
			doctorStatus: doctor.Fail, doctorDetail: "per-repo routing for Claude Code is configured, but terma's shim directory is not ahead of the agent on your PATH — sessions still use the machine-wide config",
			status: "→ per-repo routing configured, but terma's shim is not ahead of it on your PATH", statusOK: false,
		},
		{
			// Routing that is not live yet changes nothing about a repository that asks:
			// sessions here do send, through the machine-wide config.
			name:  "routing not live, but the repository's policy makes it send",
			facts: harnessFacts{status: silent, routed: true, repoAsks: true, localScope: true}, bound: true,
			route:        routeRepoDecides,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code → " + otlp + " (only where a repository asks); this repository asks",
			status: "→ connected; repositories decide what is sent", statusOK: true,
		},
		{
			name:  "silent machine-wide config, and this repository neither routes nor asks",
			facts: harnessFacts{status: silent, localScope: true}, bound: true,
			route:        routeRepoDecides,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code → " + otlp + " (only where a repository asks); this repository does not route Claude Code to its project, so its sessions send nothing",
			status: "→ no telemetry: this repository neither routes it nor asks for it — sessions here send nothing (run `terma install`)", statusOK: false,
		},
		{
			name:  "silent machine-wide config, outside any installed repository",
			facts: harnessFacts{status: silent, localScope: true}, bound: false,
			route:        routeRepoDecides,
			doctorStatus: doctor.Pass, doctorDetail: "Claude Code → " + otlp + " (only where a repository asks)",
			status: "→ connected; repositories decide what is sent", statusOK: true,
		},
		{
			// The second place the two used to disagree: an agent that cannot carry a
			// repository policy at all (Codex) was never asked by doctor whether this
			// repository has one, so doctor passed what status said sends nothing. Only a
			// hand-edited config reaches it — connect always writes Codex's signals.
			name:  "silent config for an agent with no repository scope",
			facts: harnessFacts{status: silent}, bound: true,
			route:        routeRepoDecides,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code → " + otlp + " (only where a repository asks); this repository does not route Claude Code to its project, so its sessions send nothing",
			status: "→ no telemetry: this repository neither routes it nor asks for it — sessions here send nothing (run `terma install`)", statusOK: false,
		},
		{
			name:  "not connected",
			facts: harnessFacts{}, bound: true,
			route:        routeNone,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code installed but not exporting to " + otlp,
			status: "→ not connected", statusOK: false,
		},
		{
			name:  "configuration unreadable",
			facts: harnessFacts{err: errors.New("permission denied")}, bound: true,
			route:        routeNone,
			doctorStatus: doctor.Fail, doctorDetail: "Claude Code installed but not exporting to " + otlp,
			status: "(error)", statusOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := judgeHarness(tc.facts, otlp, project)
			v.name, v.displayName = "claude", "Claude Code"
			if v.route != tc.route {
				t.Fatalf("route = %v, want %v", v.route, tc.route)
			}
			check := doctorHarnessCheck([]harnessVerdict{v}, otlp, project, tc.bound)
			if check.Status != tc.doctorStatus || check.Detail != tc.doctorDetail {
				t.Errorf("doctor = %v %q\n         want %v %q", check.Status, check.Detail, tc.doctorStatus, tc.doctorDetail)
			}
			got, ok := statusAgent(v, tc.bound)
			if got != tc.status || ok != tc.statusOK {
				t.Errorf("status = %q, %v\n         want %q, %v", got, ok, tc.status, tc.statusOK)
			}
		})
	}
}
