package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/model"
)

type personaState struct {
	allowance *model.Allowance
	otherAt   time.Time
}

// Generate builds the world for a seed. The same scenario and seed always
// produce the same world.
func Generate(sc Scenario, seed int64) World {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(seed)*7919+17))
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	w := World{Scenario: sc, Base: base, Truth: map[string]Truth{}, Sessions: map[string]model.Session{}}
	for pi := range sc.Personas {
		p := sc.Personas[pi]
		st := &personaState{allowance: model.NewAllowance(sc.Allowance[p.Account.Tier()]), otherAt: base}
		if p.Alt != nil && p.Alt.Account.Tier() != p.Account.Tier() {
			// A switch to another seat gets that seat's own limits.
			st.allowance = model.NewAllowance(sc.Allowance[p.Account.Tier()])
		}
		for d := 0; d < sc.Days; d++ {
			n := poisson(rng, p.SessionsPerDay)
			for s := 0; s < n; s++ {
				start := base.Add(time.Duration(d)*Day + time.Duration(8*60+rng.IntN(12*60))*time.Minute)
				switchAt := -1
				cur := p
				switch {
				case p.SwitchOnDay > 0 && d+1 == p.SwitchOnDay && s == 0:
					switchAt = 0 // set below once the call count is known
				case p.SwitchOnDay > 0 && (d+1 > p.SwitchOnDay || (d+1 == p.SwitchOnDay && s > 0)):
					cur = *p.Alt
				}
				sid := fmt.Sprintf("%s-d%02d-s%d", p.ID, d+1, s)
				genSession(&w, rng, sc, p, cur, st, sid, start, switchAt)
			}
		}
	}
	sort.Slice(w.Calls, func(i, j int) bool {
		if !w.Calls[i].At.Equal(w.Calls[j].At) {
			return w.Calls[i].At.Before(w.Calls[j].At)
		}
		return w.Calls[i].ID < w.Calls[j].ID
	})
	w.Reports = buildReports(&w)
	return w
}

func genSession(w *World, rng *rand.Rand, sc Scenario, base, cur Persona, st *personaState, sid string, start time.Time, switchAt int) {
	n := 1 + poisson(rng, cur.CallsPerSession)
	if switchAt == 0 {
		switchAt = n / 2
	}
	sess := newSession(sid, base.ID, cur, start)
	at := start
	var quota *model.QuotaSnapshot
	for k := 0; k < n; k++ {
		if k == switchAt && base.Alt != nil {
			cur = *base.Alt
			if sc.SnapshotPerTurn {
				w.Sessions[sess.ID] = sess
				sess = newSession(sid+"-b", base.ID, cur, at)
			}
		}
		at = at.Add(time.Duration(20+rng.IntN(100)) * time.Second)
		mdl := cur.Models[rng.IntN(len(cur.Models))]
		toks := drawTokens(rng, cur.TokenScale)
		fast := cur.Harness == model.HarnessClaude && cur.Route == model.RouteSubscription &&
			cur.Account.CreditsAvailable() && rng.Float64() < cur.FastProb
		ref := model.ListPriceUSD(mdl, fast, toks)
		c := model.Call{
			ID: fmt.Sprintf("%s-c%03d", sid, k), UserID: base.ID, SessionID: sess.ID, Harness: cur.Harness,
			At: at, Model: mdl, Tokens: toks, ReferenceUSD: ref, Entrypoint: cur.entrypoint(),
		}
		if cur.Harness == model.HarnessClaude {
			c.Speed = "normal"
			if fast {
				c.Speed = "fast"
			}
		} else {
			c.AuthMode = "swic"
			if cur.Route != model.RouteSubscription {
				c.AuthMode = "api"
			}
		}
		if cur.ExposesQuota && cur.Route == model.RouteSubscription {
			c.Quota = quota
		}
		t := Truth{Route: cur.Route}
		switch cur.Route {
		case model.RouteSubscription:
			// Allowance the seat burned elsewhere since the last call.
			if cur.OtherSurfaceUSDPerHour > 0 {
				if h := at.Sub(st.otherAt).Hours(); h > 0 {
					st.allowance.Add(at, cur.OtherSurfaceUSDPerHour*h)
				}
			}
			st.otherAt = at
			switch {
			case fast:
				t.Funding, t.MeteredUSD = model.FundingCredits, ref
			case st.allowance.Exceeds(at, ref):
				if !cur.Account.CreditsAvailable() {
					// The provider refuses the request; the developer stops for now.
					sess.Limits = append(sess.Limits, model.LimitEvent{At: at, Kind: "rate_limit"})
					w.Blocked++
					w.Sessions[sess.ID] = sess
					return
				}
				t.Funding, t.MeteredUSD = model.FundingCredits, ref
			default:
				t.Funding = model.FundingIncluded
				st.allowance.Add(at, ref)
			}
			if cur.ExposesQuota {
				// The status line re-renders after the response with the provider's
				// window fill, capped at 100 like the documented field.
				quota = quotaSnapshot(st.allowance, at)
			}
		default:
			t.Funding, t.MeteredUSD = model.FundingMetered, ref
		}
		w.Calls = append(w.Calls, c)
		w.Truth[c.ID] = t
	}
	w.Sessions[sess.ID] = sess
}

func quotaSnapshot(a *model.Allowance, at time.Time) *model.QuotaSnapshot {
	fills := a.Fills(at, 0)
	q := &model.QuotaSnapshot{At: at}
	for i, w := range a.Windows() {
		pct := math.Min(100, 100*fills[i])
		switch {
		case w.Length <= 6*Hour:
			q.FiveHourUsedPct = &pct
			q.FiveHourResetsAt = at.Add(w.Length)
		default:
			q.SevenDayUsedPct = &pct
			q.SevenDayResetsAt = at.Add(w.Length)
		}
	}
	return q
}

func newSession(id, user string, p Persona, at time.Time) model.Session {
	s := model.Session{ID: id, UserID: user, Harness: p.Harness, StartedAt: at, Hints: p.Hints}
	if p.Account != nil {
		a := *p.Account
		s.Account = &a
	}
	return s
}

// drawTokens is a typical agentic turn: a large cached prefix, a little fresh
// input, a modest completion.
func drawTokens(rng *rand.Rand, scale float64) model.Tokens {
	if scale == 0 {
		scale = 1
	}
	total := lognormal(rng, math.Log(30000*scale), 0.6)
	out := lognormal(rng, math.Log(600*scale), 0.8)
	return model.Tokens{
		Input:      int64(total * 0.10),
		CacheRead:  int64(total * 0.85),
		CacheWrite: int64(total * 0.05),
		Output:     int64(out),
	}
}

func lognormal(rng *rand.Rand, mu, sigma float64) float64 {
	return math.Exp(mu + sigma*rng.NormFloat64())
}

func poisson(rng *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	l, k, p := math.Exp(-lambda), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= l {
			return k
		}
		k++
	}
}
