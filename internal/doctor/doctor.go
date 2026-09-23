// Package doctor is the report model behind `terma doctor`: checks and actionable
// setup readiness, without estimating spend from configuration.
//
// Every onboarding failure mode that is
// not surfaced here becomes a support thread, so the checks are explicit about
// what they verified and each failure carries the one command that fixes it.
package doctor

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/style"
)

// Status is a check's outcome.
type Status int

// The outcomes. Warn is something that works today and will not for long, or works
// only in part; Skip is a check that did not apply here, which is not a pass and must
// not be counted as one. Checks are reported in the order they run, never by outcome.
const (
	Pass Status = iota
	Warn
	Fail
	Skip
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "FAIL"
	default:
		return "skip"
	}
}

// Check is one verification and what it found.
type Check struct {
	// Key identifies the check across doctor and status.
	Key    string
	Name   string
	Status Status
	Detail string
	// Fix is the command or step that turns a Fail/Warn into a Pass.
	Fix string
	// Inconclusive means verification could not establish success or failure.
	Inconclusive bool
	// Ready of Of counts agents that can export or run their hooks. Both zero means
	// the check does not report a count.
	Ready, Of int
	Duration  time.Duration
}

// Report is the full doctor output.
type Report struct {
	Checks []Check
}

// Failed reports whether any check failed outright.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Check keys shared by doctor and status.
const (
	KeyBinary  = "binary"
	KeyAuth    = "auth"
	KeyProject = "project"
	KeyHooks   = "hooks"
	// KeyAgentHooks: the agents' own hooks (session start, files touched) are wired and
	// each agent will run them — which for Codex and Antigravity takes the developer's
	// trust, given from inside the agent.
	KeyAgentHooks = "agent-hooks"
	KeyHarness    = "harness"
	// KeyRouting reports whether shell integration actually routes agent launches.
	KeyRouting = "routing"
	// KeyStatusLine: Claude Code's status line feeds terma the plan's usage windows.
	KeyStatusLine = "statusline"
	KeyScratch    = "scratch-commit"
	KeySpool      = "spool"
	KeyBackend    = "backend"
	KeyGitHubApp  = "github-app"
)

// Build assembles a report from checks.
func Build(checks []Check) Report { return Report{Checks: checks} }

// NameWidth is the column the check names are padded to. Fixed rather than measured
// so a report streamed one check at a time lines up the same as one printed at once.
const NameWidth = 24

// RenderCheck prints one check's line — and its fix, when it needs one — as soon as
// the check is known, so a slow report reads as progress rather than silence.
func RenderCheck(w io.Writer, c Check, width int) {
	p := style.For(w)
	status := fmt.Sprintf("%-4s", c.Status)
	switch c.Status {
	case Pass:
		status = p.OK(status)
	case Warn:
		status = p.Warn(status)
	case Fail:
		status = p.Fail(status)
	default:
		status = p.Dim(status)
	}
	fmt.Fprintf(w, "  %s  %-*s  %s\n", status, width, c.Name, c.Detail)
	if c.Fix != "" && c.Status != Pass {
		fmt.Fprintf(w, "        %*s  %s %s\n", width, "", p.Brand("→"), c.Fix)
	}
}

// RenderSummary names the remaining setup actions, without claiming measured coverage.
func RenderSummary(w io.Writer, r Report) {
	p := style.For(w)
	var actions []string
	seen := map[string]bool{}
	skipped := false
	for _, c := range r.Checks {
		if c.Status == Skip {
			if c.Key == KeyScratch || c.Key == KeyBackend || c.Key == KeyProject {
				skipped = true
			}
			continue
		}
		if c.Status == Pass {
			continue
		}
		action := c.Fix
		if action == "" {
			action = c.Name + ": " + c.Detail
		}
		if !seen[action] {
			actions = append(actions, action)
			seen[action] = true
		}
	}
	if len(actions) == 0 {
		if skipped {
			fmt.Fprintf(w, "\n%s — completed checks passed; some checks were skipped.\n", p.Bold("Verification incomplete"))
		} else {
			fmt.Fprintf(w, "\n%s — all checks passed.\n", p.Bold("Setup ready"))
		}
		return
	}
	fmt.Fprintf(w, "\n%s\n", p.Bold("Setup needs attention:"))
	for _, action := range actions {
		fmt.Fprintf(w, "  - %s\n", strings.TrimSpace(action))
	}
}
