// Package sim generates worlds with known truth: who each developer really is,
// which route each call really took, and what the provider's export for that
// world would say. The estimator never sees the truth; the evaluation compares
// its answers with it.
package sim

import (
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

// Persona is one developer's real situation and habits.
type Persona struct {
	ID      string
	Harness model.Harness
	// Route is the credential the provider actually bills.
	Route model.Route
	// Account is what the hook finds on disk. Nil when no login profile exists.
	Account *model.AccountSnapshot
	Hints   model.Hints
	// Entrypoint is the Claude app.entrypoint ("cli" when empty).
	Entrypoint string

	SessionsPerDay  float64
	CallsPerSession float64
	// FastProb is the chance a call runs in fast mode when the account allows it.
	FastProb float64
	Models   []string
	// TokenScale multiplies the token draws: 1 is an ordinary session.
	TokenScale float64
	// OtherSurfaceUSDPerHour is allowance the same seat burns where Terma cannot
	// see it: claude.ai chat, Cowork, another machine.
	OtherSurfaceUSDPerHour float64
	// ExposesQuota: the plan's status line carries rate_limits, so every call
	// after the first in a session sees the provider's own window fill.
	ExposesQuota bool

	// SwitchOnDay flips the persona to Alt mid-session on that 1-based day and
	// for every session after. 0 never switches.
	SwitchOnDay int
	Alt         *Persona
}

func (p Persona) entrypoint() string {
	if p.Entrypoint == "" {
		return "cli"
	}
	return p.Entrypoint
}

// Scenario is a world to generate.
type Scenario struct {
	Name string
	// Days is the whole timeline. WarmupDays at the start are generated and
	// seen by the estimator but not scored and produce no exports: they stand
	// for the history a running installation has, so that windows do not start
	// empty on the first scored day.
	Days       int
	WarmupDays int
	Personas   []Persona
	// Allowance is the true included capacity per tier, in list USD.
	Allowance map[string][]model.Window
	// ReportIncludesAllowanceRows: the Claude Team export lists user-model-days
	// with zero credit spend (true on the export we downloaded; the docs read as
	// if only credit spend appears). Both shapes must reconcile.
	ReportIncludesAllowanceRows bool
	// SnapshotPerTurn models the hook re-reading the account state at every Stop,
	// so a mid-session /login shows up as a new session record. Without it the
	// SessionStart snapshot goes stale.
	SnapshotPerTurn bool
	// EstimatorAllowanceScale mis-specifies the estimator's capacity table
	// relative to the truth (1 = it knows the real limits).
	EstimatorAllowanceScale float64
}

// Truth is what really happened to one call.
type Truth struct {
	Route      model.Route
	Funding    model.Funding
	MeteredUSD float64
}

// World is one generated timeline.
type World struct {
	Scenario Scenario
	Base     time.Time
	Calls    []model.Call
	Truth    map[string]Truth
	Sessions map[string]model.Session
	Reports  []model.ReportRow
	// Blocked counts calls the provider refused for want of allowance and credits.
	Blocked int
}

// EstimatorConfig is the belief table the estimator should run with for this
// scenario: the truth scaled by EstimatorAllowanceScale.
func (sc Scenario) EstimatorConfig() model.Config {
	scale := sc.EstimatorAllowanceScale
	if scale == 0 {
		scale = 1
	}
	cfg := model.Config{Allowance: map[string][]model.Window{}}
	for tier, ws := range sc.Allowance {
		for _, w := range ws {
			cfg.Allowance[tier] = append(cfg.Allowance[tier], model.Window{Length: w.Length, CapacityUSD: w.CapacityUSD * scale})
		}
	}
	cfg.Default = cfg.Allowance["team_tier_1"]
	return cfg
}

// Hours and days, for readability in presets.
const (
	Hour = time.Hour
	Day  = 24 * time.Hour
)
