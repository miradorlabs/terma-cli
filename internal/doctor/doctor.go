// Package doctor runs `terma doctor`'s checks in order and judges the verdicts status
// shares with it, reaching the spool and the platform's APIs only through Probes.
package doctor

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/ui/style"
)

// Status is a check's outcome.
type Status int

// The outcomes. Warn works only in part or not for long; Skip did not apply and is never
// counted as a pass.
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
	Key    string
	Name   string
	Status Status
	Detail string
	// Fix is the command or step that turns a Fail/Warn into a Pass.
	Fix          string
	Inconclusive bool
	// Ready of Of counts agents that can export or run their hooks; both zero means no count.
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
	KeyBinary        = "binary"
	KeyState         = "state"
	KeyAuth          = "auth"
	KeyProject       = "project"
	KeyHooks         = "hooks"
	KeyAgentHooks    = "agent-hooks"
	KeyHarness       = "harness"
	KeyCompatibility = "compatibility"
	KeyRouting       = "routing"
	KeyStatusLine    = "statusline"
	KeyScratch       = "scratch-commit"
	KeySpool         = "spool"
	KeyBackend       = "backend"
	KeyGitHubApp     = "github-app"
)

// Build assembles a report from checks.
func Build(checks []Check) Report { return Report{Checks: checks} }

// NameWidth is the fixed column check names pad to, so a streamed report lines up.
const NameWidth = 24

// RenderCheck prints one check's line, and its fix when it needs one.
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
	fmt.Fprintf(w, "  %s  %-*s  %s\n", status, width, c.Name, p.Commands(c.Detail))
	if c.Fix != "" && c.Status != Pass {
		fmt.Fprintf(w, "        %*s  %s %s\n", width, "", p.Brand("→"), fixText(p, c.Fix))
	}
}

// fixText draws the terma command a fix leads with, and any quoted in it, as commands.
func fixText(p style.Palette, fix string) string {
	if !strings.HasPrefix(fix, "terma ") {
		return p.Commands(fix)
	}
	head, tail := fix, ""
	if i := strings.IndexAny(fix, "(—;"); i > 0 {
		head = strings.TrimRight(fix[:i], " ")
		tail = fix[len(head):]
	}
	return p.Command(head) + p.Commands(tail)
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
		fmt.Fprintf(w, "  - %s\n", fixText(p, strings.TrimSpace(action)))
	}
}
