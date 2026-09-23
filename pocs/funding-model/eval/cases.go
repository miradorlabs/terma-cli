package eval

import "fmt"

// Case is what a scenario must achieve over its seeds. The numbers are the
// current envelope, tightened as the estimator improves; a case failing means
// a change made the estimate worse on the thing that scenario isolates.
type Case struct {
	Scenario string
	// MaxColdDelta bounds the worst-seed gap before any report is seen.
	MaxColdDelta float64
	// MinColdAccuracy bounds the worst-seed share of calls whose most likely
	// funding is the true one.
	MinColdAccuracy float64
	// MaxWarmDelta bounds the worst-seed second-half gap after learning.
	MaxWarmDelta float64
	// WarmImproves requires the mean second-half gap to shrink after learning.
	WarmImproves bool
	// MaxWarmRegression bounds how much the mean second-half gap may grow after
	// learning where the cold estimate was already close. Some regression is
	// expected: a calibration fitted on the first week, before the seven-day
	// window saturates, is slightly off for the second.
	MaxWarmRegression float64
	// MinNaiveDelta guards the premise: today's list-price figure must be
	// materially wrong in this scenario, or it is not testing anything.
	MinNaiveDelta float64
}

// Cases are the acceptance envelope for the presets.
var Cases = []Case{
	{Scenario: "team-allowance-only", MaxColdDelta: 0.03, MinColdAccuracy: 0.98, MaxWarmDelta: 0.03, MinNaiveDelta: 0.95},
	{Scenario: "team-credits", MaxColdDelta: 0.04, MinColdAccuracy: 0.94, MaxWarmDelta: 0.06, MaxWarmRegression: 0.01, MinNaiveDelta: 0.20},
	{Scenario: "team-credits-wrong-table", MaxColdDelta: 0.10, MinColdAccuracy: 0.92, MaxWarmDelta: 0.10, WarmImproves: true, MinNaiveDelta: 0.20},
	{Scenario: "team-credits-sparse-export", MaxColdDelta: 0.11, MinColdAccuracy: 0.92, MaxWarmDelta: 0.11, MaxWarmRegression: 0.01, MinNaiveDelta: 0.20},
	{Scenario: "fast-mode", MaxColdDelta: 0.01, MinColdAccuracy: 0.95, MaxWarmDelta: 0.01},
	{Scenario: "api-key-mix", MaxColdDelta: 0.22, MaxWarmDelta: 0.09, WarmImproves: true, MinNaiveDelta: 0.30},
	{Scenario: "login-switch", MaxColdDelta: 0.14, MinColdAccuracy: 0.95, MaxWarmDelta: 0.16, WarmImproves: true},
	{Scenario: "login-switch-stale-snapshot", MaxColdDelta: 0.14, MaxWarmDelta: 0.15},
	{Scenario: "hidden-surface", MaxColdDelta: 0.13, MinColdAccuracy: 0.92, MaxWarmDelta: 0.06, WarmImproves: true, MinNaiveDelta: 0.20},
	{Scenario: "pro-max-quota", MaxColdDelta: 0.07, MinColdAccuracy: 0.92, MaxWarmDelta: 0.07, MaxWarmRegression: 0.01, MinNaiveDelta: 0.20},
	{Scenario: "codex-business", MaxColdDelta: 0.04, MinColdAccuracy: 0.70, MaxWarmDelta: 0.06, MaxWarmRegression: 0.015},
	{Scenario: "mixed-org", MaxColdDelta: 0.04, MinColdAccuracy: 0.90, MaxWarmDelta: 0.04, MaxWarmRegression: 0.01, MinNaiveDelta: 0.30},
	{Scenario: "team-credits-no-statusline", MaxColdDelta: 0.08, MinColdAccuracy: 0.60, MaxWarmDelta: 0.14, MaxWarmRegression: 0.02, MinNaiveDelta: 0.20},
	{Scenario: "hidden-surface-no-statusline", MaxColdDelta: 0.60, MaxWarmDelta: 0.14, WarmImproves: true, MinNaiveDelta: 0.20},
}

// Check returns every way a summary misses its case. Nil means it passes.
func Check(c Case, s Summary) []string {
	var fails []string
	f := func(format string, a ...any) { fails = append(fails, fmt.Sprintf(format, a...)) }
	if c.MaxColdDelta > 0 && s.Worst.ColdDelta > c.MaxColdDelta {
		f("cold delta %.3f > %.3f", s.Worst.ColdDelta, c.MaxColdDelta)
	}
	if c.MinColdAccuracy > 0 && s.Worst.ColdAccuracy < c.MinColdAccuracy {
		f("cold accuracy %.3f < %.3f", s.Worst.ColdAccuracy, c.MinColdAccuracy)
	}
	if c.MaxWarmDelta > 0 && s.Worst.SecondWarmDelta > c.MaxWarmDelta {
		f("warm delta %.3f > %.3f", s.Worst.SecondWarmDelta, c.MaxWarmDelta)
	}
	if c.WarmImproves && s.Mean.SecondWarmDelta > s.Mean.SecondColdDelta {
		f("learning made it worse: warm %.3f vs cold %.3f", s.Mean.SecondWarmDelta, s.Mean.SecondColdDelta)
	}
	if c.MaxWarmRegression > 0 && s.Mean.SecondWarmDelta > s.Mean.SecondColdDelta+c.MaxWarmRegression {
		f("learning regressed by %.3f (warm %.3f vs cold %.3f), more than %.3f", s.Mean.SecondWarmDelta-s.Mean.SecondColdDelta, s.Mean.SecondWarmDelta, s.Mean.SecondColdDelta, c.MaxWarmRegression)
	}
	if c.MinNaiveDelta > 0 && s.Mean.NaiveDelta < c.MinNaiveDelta {
		f("naive delta %.3f < %.3f: scenario does not exercise the premise", s.Mean.NaiveDelta, c.MinNaiveDelta)
	}
	return fails
}

// CaseFor finds the case for a scenario.
func CaseFor(name string) (Case, bool) {
	for _, c := range Cases {
		if c.Scenario == name {
			return c, true
		}
	}
	return Case{}, false
}
