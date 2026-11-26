// Package doctor runs `terma doctor`'s checks in order and judges the verdicts status
// shares with it, reaching the spool and the platform's APIs only through Probes.
package doctor

import "time"

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
	KeyAuth          = "auth"
	KeyProject       = "team"
	KeyHooks         = "hooks"
	KeyUserHooks     = "user-hooks"
	KeyAgentHooks    = "agent-hooks"
	KeyHarness       = "harness"
	KeyCompatibility = "compatibility"
	KeyRouting       = "routing"
	KeyStatusLine    = "statusline"
	KeySpool         = "spool"
	KeyOutbox        = "outbox"
	KeyBackend       = "backend"
	KeyGitHubApp     = "github-app"
)

// Build assembles a report from checks.
func Build(checks []Check) Report { return Report{Checks: checks} }
