package model

import "time"

// Window is one rolling usage limit, expressed in reference USD so that calls
// on different models can be summed. Providers meter their windows in their
// own units; USD at list price is the closest observable proxy.
type Window struct {
	Length      time.Duration
	CapacityUSD float64
}

type usageEntry struct {
	at  time.Time
	usd float64
}

// Allowance tracks rolling-window usage. The simulator uses one as the truth
// of a seat's limits; the estimator uses one as its guess of how full those
// limits are from the calls it has seen.
//
// Usage must be added in time order: each window keeps a running sum and a
// start index that only move forward, so every operation is amortised O(1)
// and a fit can replay thousands of calls per candidate without cost. A time
// earlier than the latest seen is treated as the latest.
type Allowance struct {
	windows []Window
	hist    []usageEntry
	start   []int
	sum     []float64
	latest  time.Time
}

// NewAllowance tracks the given windows. With no windows nothing ever fills.
func NewAllowance(windows []Window) *Allowance {
	return &Allowance{windows: append([]Window(nil), windows...), start: make([]int, len(windows)), sum: make([]float64, len(windows))}
}

// Windows returns the tracked windows.
func (a *Allowance) Windows() []Window { return a.windows }

func (a *Allowance) clock(at time.Time) time.Time {
	if at.After(a.latest) {
		a.latest = at
	}
	return a.latest
}

// advance drops entries that have left each window as of the clock.
func (a *Allowance) advance(now time.Time) {
	for i, w := range a.windows {
		cut := now.Add(-w.Length)
		for a.start[i] < len(a.hist) && !a.hist[a.start[i]].at.After(cut) {
			a.sum[i] -= a.hist[a.start[i]].usd
			a.start[i]++
		}
	}
	// Compact once every window has moved past the same prefix.
	min := len(a.hist)
	for _, s := range a.start {
		if s < min {
			min = s
		}
	}
	if min > 1024 {
		a.hist = append(a.hist[:0], a.hist[min:]...)
		for i := range a.start {
			a.start[i] -= min
		}
	}
}

// Add records usage at a time.
func (a *Allowance) Add(at time.Time, usd float64) {
	if usd <= 0 {
		return
	}
	now := a.clock(at)
	a.advance(now)
	a.hist = append(a.hist, usageEntry{at: now, usd: usd})
	for i := range a.sum {
		a.sum[i] += usd
	}
}

// Fill is the highest ratio of used to capacity across the windows if usd were
// added at that time. 1 means exactly at a limit.
func (a *Allowance) Fill(at time.Time, usd float64) float64 {
	now := a.clock(at)
	a.advance(now)
	var fill float64
	for i, w := range a.windows {
		if w.CapacityUSD <= 0 {
			continue
		}
		if f := (a.sum[i] + usd) / w.CapacityUSD; f > fill {
			fill = f
		}
	}
	return fill
}

// Fills is every window's fill ratio if usd were added at that time, in
// window order: the shape a provider reports per window.
func (a *Allowance) Fills(at time.Time, usd float64) []float64 {
	now := a.clock(at)
	a.advance(now)
	out := make([]float64, len(a.windows))
	for i, w := range a.windows {
		if w.CapacityUSD > 0 {
			out[i] = (a.sum[i] + usd) / w.CapacityUSD
		}
	}
	return out
}

// Exceeds says whether adding usd at that time would pass a limit.
func (a *Allowance) Exceeds(at time.Time, usd float64) bool { return a.Fill(at, usd) > 1 }
