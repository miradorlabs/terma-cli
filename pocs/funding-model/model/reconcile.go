package model

import (
	"math"
	"sort"
	"time"
)

// DayKey is the grain shared by every provider export we have inspected.
type DayKey struct {
	UserID string
	Model  string
	Day    time.Time
}

// Reconciliation compares one exported line with the calls Terma saw under it.
type Reconciliation struct {
	Kind        DayKey
	ReportKind  ReportKind
	HasRow      bool
	Requests    int
	ReportedUSD float64 // 0 when the export has no line for calls we saw
	// EstimatedUSD is the sum over the day's calls of the expected spend this
	// report kind would meter: credits for a subscription export, everything
	// for a Console export.
	EstimatedUSD float64
	// SubscriptionRefUSD is the reference cost of the day's normal-speed calls,
	// weighted by how likely each was on a subscription route. It is the
	// denominator of the learned credit share. FastRefUSD is the same for fast
	// calls, whose credit spend is known by rule and must not teach the share.
	SubscriptionRefUSD float64
	FastRefUSD         float64
	// LearnableCalls counts calls whose route came from a learnable default.
	LearnableCalls int
	// Fills are the day's normal-speed calls in time order with their
	// subscription-weighted reference cost, the material the calibration is
	// fitted on by replaying them through the tier's windows.
	Fills   []FillObs
	Tier    string
	CallIDs []string
	// Allocated spreads ReportedUSD over the calls, in proportion to each call's
	// expected metered spend. It is an allocation, not an observation.
	Allocated map[string]float64
}

// FillObs is one normal-speed call's time and subscription-weighted reference
// cost.
type FillObs struct {
	At     time.Time
	RefUSD float64
}

// ObservedCreditsUSD is the export's credit spend attributable to normal-speed
// calls: the reported figure less what fast mode is known to have cost.
func (r Reconciliation) ObservedCreditsUSD() float64 {
	return clamp(r.ReportedUSD-r.FastRefUSD, 0, r.SubscriptionRefUSD)
}

// Result is one report reconciled against one set of calls.
type Result struct {
	Rows []Reconciliation
	// ReportedUSD and EstimatedUSD sum over rows that had calls.
	ReportedUSD, EstimatedUSD float64
	// UnmatchedUSD is spend the export shows for user-days Terma saw no calls in:
	// other machines, other surfaces, or collection gaps.
	UnmatchedUSD float64
}

// Delta is what the customer would have been shown minus what they were charged.
func (r Result) Delta() float64 { return r.EstimatedUSD - r.ReportedUSD }

// weight is the share of a call's reference cost this report kind would meter.
func weight(kind ReportKind, est Estimate) float64 {
	if kind == ReportClaudeConsoleUsage {
		return est.P[FundingMetered]
	}
	return est.P[FundingCredits]
}

// Reconcile matches a report against calls. Only calls from the report's
// harness and from users the report covers take part: a user who appears
// nowhere in the export is not a member of what it describes (an individual
// plan next to a Team export), and their silence says nothing. A row with no
// calls is unmatched; a covered user's day with no row is reconciled against
// zero, which is the export's statement for that day.
func Reconcile(kind ReportKind, rows []ReportRow, calls []Call, ests map[string]Estimate) Result {
	byKey := map[DayKey]ReportRow{}
	covered := map[string]bool{}
	for _, r := range rows {
		if r.Kind != kind {
			continue
		}
		byKey[DayKey{r.UserID, r.Model, Day(r.Day)}] = r
		covered[r.UserID] = true
	}
	groups := map[DayKey][]Call{}
	for _, c := range calls {
		if c.Harness != kind.Harness() || !covered[c.UserID] {
			continue
		}
		k := DayKey{c.UserID, c.Model, Day(c.At)}
		groups[k] = append(groups[k], c)
	}
	keys := make([]DayKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if !a.Day.Equal(b.Day) {
			return a.Day.Before(b.Day)
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		return a.Model < b.Model
	})
	var res Result
	for _, k := range keys {
		cs := groups[k]
		rec := Reconciliation{Kind: k, ReportKind: kind, Allocated: map[string]float64{}}
		if row, ok := byKey[k]; ok {
			rec.HasRow, rec.Requests, rec.ReportedUSD = true, row.Requests, row.SpendUSD
			delete(byKey, k)
		}
		var wsum, refsum float64
		ws := make([]float64, len(cs))
		for i, c := range cs {
			est := ests[c.ID]
			ws[i] = weight(kind, est) * c.ReferenceUSD
			wsum += ws[i]
			refsum += c.ReferenceUSD
			rec.EstimatedUSD += ws[i]
			if c.Speed == "fast" {
				rec.FastRefUSD += est.PSubscription() * c.ReferenceUSD
			} else {
				w := est.PSubscription() * c.ReferenceUSD
				rec.SubscriptionRefUSD += w
				rec.Fills = append(rec.Fills, FillObs{At: c.At, RefUSD: w})
			}
			if rec.Tier == "" {
				rec.Tier = est.Tier
			}
			if est.RouteLearnable {
				rec.LearnableCalls++
			}
			rec.CallIDs = append(rec.CallIDs, c.ID)
		}
		for i, c := range cs {
			switch {
			case wsum > 0:
				rec.Allocated[c.ID] = rec.ReportedUSD * ws[i] / wsum
			case refsum > 0:
				rec.Allocated[c.ID] = rec.ReportedUSD * c.ReferenceUSD / refsum
			}
		}
		res.ReportedUSD += rec.ReportedUSD
		res.EstimatedUSD += rec.EstimatedUSD
		res.Rows = append(res.Rows, rec)
	}
	for _, r := range byKey {
		res.UnmatchedUSD += r.SpendUSD
	}
	return res
}

// Learn folds a reconciliation into the per-user priors.
//
// Route: a line in a subscription export is a subscription observation, a line
// in Console usage an API-key observation, and a user-day with Claude calls but
// no Console line leans towards the subscription. Each counts in proportion to
// the day's calls whose route was learnable, so a day spent on a documented
// configuration teaches nothing about an ambiguous one.
//
// Credit share: the export's spend, less what fast mode is known to have cost,
// over the day's normal-speed subscription reference cost.
//
// Calibration: across a user's reconciled days, the (scale, hidden rate) that
// makes the allowance view's predicted credit spend match the export's, found
// by replaying the days through the tier's windows for each candidate on a
// grid, so the result is bounded and explainable. It is blended with the
// previous fit in proportion to the days behind each.
func (e *Estimator) Learn(res Result) {
	byUser := map[string][]Reconciliation{}
	for _, rec := range res.Rows {
		if len(rec.CallIDs) == 0 {
			continue
		}
		pr := e.prior(rec.Kind.UserID)
		share := float64(rec.LearnableCalls) / float64(len(rec.CallIDs))
		switch rec.ReportKind {
		case ReportClaudeConsoleUsage:
			if rec.HasRow {
				pr.addRoute(RouteAPIKey, share)
			} else {
				pr.addRoute(RouteSubscription, 0.5*share)
			}
		default:
			if rec.HasRow && (rec.Requests > 0 || rec.ReportedUSD > 0) {
				pr.addRoute(RouteSubscription, share)
			}
			if rec.SubscriptionRefUSD > 0 {
				pr.addCreditShare(rec.ObservedCreditsUSD() / rec.SubscriptionRefUSD)
				byUser[rec.Kind.UserID] = append(byUser[rec.Kind.UserID], rec)
			}
		}
	}
	for user, rows := range byUser {
		if len(rows) < minFitRows {
			continue
		}
		scale, rate := fitCalibration(rows, e.windowsFor(rows[0].Tier))
		pr := e.prior(user)
		days := float64(len(rows))
		total := pr.FitDays + days
		pr.FillScale = (pr.FillScale*pr.FitDays + scale*days) / total
		pr.HiddenUSDPerHour = (pr.HiddenUSDPerHour*pr.FitDays + rate*days) / total
		pr.FitDays = math.Min(total, fitMemory)
	}
}

const (
	minFitRows = 3
	fitMemory  = 30.0
)

var (
	fitScales = []float64{0.5, 0.7, 0.85, 1, 1.2, 1.4, 1.7, 2, 2.5}
	fitRates  = []float64{0, 0.05, 0.1, 0.2, 0.35, 0.5, 0.8, 1.2, 2}
	// fitGain is the relative error reduction a calibration must buy before it
	// displaces the identity: a well-specified view is left alone, and a user
	// who never nears a limit does not get an extreme fit from noise.
	fitGain = 0.25
)

// fitCalibration replays a user's reconciled days through the windows for each
// candidate and keeps the one whose predicted credit spend is closest to the
// export's, day by day. The replay feeds the windows the expected included
// share of each call, as the live estimator does.
//
// Days are split by parity: the candidate is chosen on odd days and accepted
// only if it also improves even days by fitGain over the identity. A
// calibration that only explains the days it was fitted on is noise, and noise
// applied to next week's calls is worse than the uncalibrated view.
func fitCalibration(rows []Reconciliation, windows []Window) (scale, rate float64) {
	type obs struct {
		at  time.Time
		usd float64
		row int
	}
	var all []obs
	for i, r := range rows {
		for _, f := range r.Fills {
			all = append(all, obs{f.At, f.RefUSD, i})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	validation := make([]bool, len(rows))
	for i, r := range rows {
		validation[i] = r.Kind.Day.YearDay()%2 == 0
	}
	errOf := func(scale, rate float64) (train, val float64) {
		tr := NewAllowance(windows)
		pred := make([]float64, len(rows))
		var last time.Time
		for _, o := range all {
			if rate > 0 && !last.IsZero() {
				if h := o.at.Sub(last).Hours(); h > 0 {
					tr.Add(o.at, rate*h)
				}
			}
			last = o.at
			p := pOver(tr.Fill(o.at, o.usd), scale)
			pred[o.row] += p * o.usd
			tr.Add(o.at, o.usd*(1-p))
		}
		for i, r := range rows {
			e := math.Abs(pred[i] - r.ObservedCreditsUSD())
			if validation[i] {
				val += e
			} else {
				train += e
			}
		}
		return train, val
	}
	baseTrain, baseVal := errOf(1, 0)
	best, bestTrain, bestVal := [2]float64{1, 0}, baseTrain, baseVal
	for _, sc := range fitScales {
		for _, rt := range fitRates {
			if train, val := errOf(sc, rt); train < bestTrain-1e-9 {
				best, bestTrain, bestVal = [2]float64{sc, rt}, train, val
			}
		}
	}
	if baseVal-bestVal < fitGain*baseVal {
		return 1, 0
	}
	return best[0], best[1]
}
