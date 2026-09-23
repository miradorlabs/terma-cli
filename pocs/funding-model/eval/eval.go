// Package eval runs the estimator over simulated worlds and measures the gap
// between what it would have shown and what the provider charged.
package eval

import (
	"math"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
	"github.com/miradorlabs/terma-cli/pocs/funding-model/sim"
)

// Metrics is one run's scorecard. Deltas are absolute gaps in USD divided by
// the period's total reference cost, so a scenario with no metered spend still
// has a meaningful number.
type Metrics struct {
	Scenario string
	Seed     int64
	Calls    int
	Blocked  int

	ReferenceUSD   float64 // what Terma shows today, summed
	TrueMeteredUSD float64 // what actually left the account

	// Cold: estimator with nothing learned, over the whole period.
	ColdEstimatedUSD float64
	ColdDelta        float64
	ColdAccuracy     float64 // argmax funding == truth
	ColdBrier        float64 // on the metered indicator

	// Naive: today's behaviour, everything counted at reference.
	NaiveDelta float64

	// Reconciled: on the first half's reconcilable calls, how far the spend
	// allocated from the export lands from the truth per call.
	AllocationError float64
	UnmatchedUSD    float64

	// Second half, cold versus after learning from the first half's exports.
	SecondColdDelta    float64
	SecondWarmDelta    float64
	SecondWarmAccuracy float64
}

// periods splits a world into the scored range: the first half is what gets
// reconciled and learned from, the second half is where learning is judged.
func periods(w sim.World) (scored, split time.Time) {
	scored = w.Base.AddDate(0, 0, w.Scenario.WarmupDays)
	split = scored.AddDate(0, 0, (w.Scenario.Days-w.Scenario.WarmupDays)/2)
	return scored, split
}

// Run evaluates one scenario for one seed.
func Run(sc sim.Scenario, seed int64) Metrics {
	w := sim.Generate(sc, seed)
	cfg := sc.EstimatorConfig()
	scored, split := periods(w)

	cold := model.New(cfg)
	coldEsts := estimateAll(cold, w)

	m := Metrics{Scenario: sc.Name, Seed: seed, Blocked: w.Blocked}
	var coldEst, secondRef, secondTrue, secondCold float64
	var hits, brier float64
	for _, c := range w.Calls {
		if c.At.Before(scored) {
			continue
		}
		m.Calls++
		t := w.Truth[c.ID]
		e := coldEsts[c.ID]
		m.ReferenceUSD += c.ReferenceUSD
		m.TrueMeteredUSD += t.MeteredUSD
		coldEst += e.ExpectedMeteredUSD
		if e.Best() == t.Funding {
			hits++
		}
		ind := 0.0
		if t.Funding.Metered() {
			ind = 1
		}
		brier += (e.PMetered() - ind) * (e.PMetered() - ind)
		if !c.At.Before(split) {
			secondRef += c.ReferenceUSD
			secondTrue += t.MeteredUSD
			secondCold += e.ExpectedMeteredUSD
		}
	}
	m.ColdEstimatedUSD = coldEst
	m.ColdDelta = ratio(math.Abs(coldEst-m.TrueMeteredUSD), m.ReferenceUSD)
	m.NaiveDelta = ratio(math.Abs(m.ReferenceUSD-m.TrueMeteredUSD), m.ReferenceUSD)
	m.ColdAccuracy = ratio(hits, float64(m.Calls))
	m.ColdBrier = ratio(brier, float64(m.Calls))

	// Reconcile the first half against every export and learn from it.
	var firstCalls []model.Call
	var firstRows []model.ReportRow
	for _, c := range w.Calls {
		if !c.At.Before(scored) && c.At.Before(split) {
			firstCalls = append(firstCalls, c)
		}
	}
	for _, r := range w.Reports {
		if r.Day.Before(split) {
			firstRows = append(firstRows, r)
		}
	}
	var allocErr, allocTrue float64
	for _, kind := range []model.ReportKind{model.ReportClaudeTeamSpend, model.ReportClaudeConsoleUsage, model.ReportOpenAICredits} {
		res := model.Reconcile(kind, firstRows, firstCalls, coldEsts)
		cold.Learn(res)
		m.UnmatchedUSD += res.UnmatchedUSD
		for _, row := range res.Rows {
			for _, id := range row.CallIDs {
				truth := 0.0
				if t := w.Truth[id]; t.Funding.Metered() && metersUnder(kind, t) {
					truth = t.MeteredUSD
				}
				allocErr += math.Abs(row.Allocated[id] - truth)
				allocTrue += truth
			}
		}
	}
	m.AllocationError = ratio(allocErr, math.Max(allocTrue, 0.01))

	// Second half with the learned priors and a fresh allowance view.
	warm := model.New(cfg)
	warm.SetPriors(cold.Priors())
	warmEsts := estimateAll(warm, w)
	var secondWarm, warmHits, secondN float64
	for _, c := range w.Calls {
		if c.At.Before(split) {
			continue
		}
		secondN++
		e := warmEsts[c.ID]
		secondWarm += e.ExpectedMeteredUSD
		if e.Best() == w.Truth[c.ID].Funding {
			warmHits++
		}
	}
	m.SecondColdDelta = ratio(math.Abs(secondCold-secondTrue), secondRef)
	m.SecondWarmDelta = ratio(math.Abs(secondWarm-secondTrue), secondRef)
	m.SecondWarmAccuracy = ratio(warmHits, secondN)
	return m
}

func metersUnder(kind model.ReportKind, t sim.Truth) bool {
	switch kind {
	case model.ReportClaudeConsoleUsage:
		return t.Route == model.RouteAPIKey
	default:
		return t.Route == model.RouteSubscription
	}
}

// estimateAll feeds the world's calls to an estimator in time order.
func estimateAll(e *model.Estimator, w sim.World) map[string]model.Estimate {
	out := make(map[string]model.Estimate, len(w.Calls))
	for _, c := range w.Calls {
		out[c.ID] = e.Estimate(w.Sessions[c.SessionID], c)
	}
	return out
}

func ratio(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return num / den
}

// Summary aggregates a scenario over seeds.
type Summary struct {
	Scenario string
	Seeds    int
	Mean     Metrics
	// Worst is the 90th percentile per field (10th for accuracies): the envelope
	// a case bounds. A maximum would grow with every seed added and say more
	// about the seed count than the estimator.
	Worst Metrics
}

// RunMany evaluates a scenario over seeds 1..n.
func RunMany(sc sim.Scenario, n int) Summary {
	var runs []Metrics
	for s := 1; s <= n; s++ {
		runs = append(runs, Run(sc, int64(s)))
	}
	sum := Summary{Scenario: sc.Name, Seeds: n, Mean: Metrics{Scenario: sc.Name}, Worst: Metrics{Scenario: sc.Name}}
	f := float64(n)
	mean := func(get func(Metrics) float64) float64 {
		var t float64
		for _, r := range runs {
			t += get(r)
		}
		return t / f
	}
	high := func(get func(Metrics) float64) float64 { return percentile(runs, get, 0.9) }
	low := func(get func(Metrics) float64) float64 { return percentile(runs, get, 0.1) }
	mm, ww := &sum.Mean, &sum.Worst
	for _, r := range runs {
		mm.Calls += r.Calls
		mm.Blocked += r.Blocked
	}
	mm.Calls /= n
	mm.Blocked /= n
	mm.ReferenceUSD = mean(func(m Metrics) float64 { return m.ReferenceUSD })
	mm.TrueMeteredUSD = mean(func(m Metrics) float64 { return m.TrueMeteredUSD })
	mm.ColdEstimatedUSD = mean(func(m Metrics) float64 { return m.ColdEstimatedUSD })
	mm.ColdDelta = mean(func(m Metrics) float64 { return m.ColdDelta })
	mm.NaiveDelta = mean(func(m Metrics) float64 { return m.NaiveDelta })
	mm.ColdAccuracy = mean(func(m Metrics) float64 { return m.ColdAccuracy })
	mm.ColdBrier = mean(func(m Metrics) float64 { return m.ColdBrier })
	mm.AllocationError = mean(func(m Metrics) float64 { return m.AllocationError })
	mm.UnmatchedUSD = mean(func(m Metrics) float64 { return m.UnmatchedUSD })
	mm.SecondColdDelta = mean(func(m Metrics) float64 { return m.SecondColdDelta })
	mm.SecondWarmDelta = mean(func(m Metrics) float64 { return m.SecondWarmDelta })
	mm.SecondWarmAccuracy = mean(func(m Metrics) float64 { return m.SecondWarmAccuracy })
	ww.ColdDelta = high(func(m Metrics) float64 { return m.ColdDelta })
	ww.NaiveDelta = high(func(m Metrics) float64 { return m.NaiveDelta })
	ww.ColdAccuracy = low(func(m Metrics) float64 { return m.ColdAccuracy })
	ww.ColdBrier = high(func(m Metrics) float64 { return m.ColdBrier })
	ww.AllocationError = high(func(m Metrics) float64 { return m.AllocationError })
	ww.SecondColdDelta = high(func(m Metrics) float64 { return m.SecondColdDelta })
	ww.SecondWarmDelta = high(func(m Metrics) float64 { return m.SecondWarmDelta })
	ww.SecondWarmAccuracy = low(func(m Metrics) float64 { return m.SecondWarmAccuracy })
	return sum
}

// percentile is the nearest-rank percentile of a field over runs.
func percentile(runs []Metrics, get func(Metrics) float64, q float64) float64 {
	vals := make([]float64, len(runs))
	for i, r := range runs {
		vals[i] = get(r)
	}
	sort.Float64s(vals)
	i := int(math.Ceil(q*float64(len(vals)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(vals) {
		i = len(vals) - 1
	}
	return vals[i]
}

// Scenarios resolves "all" or a comma list of preset names, in preset order.
func Scenarios(names []string) []sim.Scenario {
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		return sim.Presets
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []sim.Scenario
	for _, s := range sim.Presets {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// UserBreakdown is one user's second half: what was true and what each
// estimator would have shown.
type UserBreakdown struct {
	UserID                string
	Calls, CreditCalls    int
	ReferenceUSD, TrueUSD float64
	ColdUSD, WarmUSD      float64
	Prior                 model.Prior
}

// Breakdown explains one seed: the priors learned from the first half and the
// per-user second-half figures. It is the tool for asking why a case failed.
func Breakdown(sc sim.Scenario, seed int64) []UserBreakdown {
	w := sim.Generate(sc, seed)
	cfg := sc.EstimatorConfig()
	scored, split := periods(w)
	cold := model.New(cfg)
	ests := estimateAll(cold, w)
	var firstCalls []model.Call
	var firstRows []model.ReportRow
	for _, c := range w.Calls {
		if !c.At.Before(scored) && c.At.Before(split) {
			firstCalls = append(firstCalls, c)
		}
	}
	for _, r := range w.Reports {
		if r.Day.Before(split) {
			firstRows = append(firstRows, r)
		}
	}
	for _, kind := range []model.ReportKind{model.ReportClaudeTeamSpend, model.ReportClaudeConsoleUsage, model.ReportOpenAICredits} {
		cold.Learn(model.Reconcile(kind, firstRows, firstCalls, ests))
	}
	warm := model.New(cfg)
	warm.SetPriors(cold.Priors())
	wests := estimateAll(warm, w)
	by := map[string]*UserBreakdown{}
	for _, c := range w.Calls {
		if c.At.Before(split) {
			continue
		}
		b := by[c.UserID]
		if b == nil {
			b = &UserBreakdown{UserID: c.UserID}
			if p := cold.Priors()[c.UserID]; p != nil {
				b.Prior = *p
			}
			by[c.UserID] = b
		}
		b.Calls++
		b.ReferenceUSD += c.ReferenceUSD
		b.TrueUSD += w.Truth[c.ID].MeteredUSD
		if w.Truth[c.ID].Funding == model.FundingCredits {
			b.CreditCalls++
		}
		b.ColdUSD += ests[c.ID].ExpectedMeteredUSD
		b.WarmUSD += wests[c.ID].ExpectedMeteredUSD
	}
	out := make([]UserBreakdown, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out
}
