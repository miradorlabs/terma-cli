package model

import (
	"math"
	"strings"
	"time"
)

// Prior is what reconciliation has taught the estimator about one user. The
// zero value has learned nothing; defaults live in the estimator so that the
// learned evidence and the assumptions stay separable.
type Prior struct {
	// RouteObs are learned pseudo-counts per route, from days the user appeared
	// in a subscription export or in Console usage while running a configuration
	// the estimator could not decide from rules alone.
	RouteObs map[Route]float64
	// CreditsA and CreditsB are Beta pseudo-counts over the share of a user's
	// normal-speed subscription spend that turned out to be usage credits.
	CreditsA, CreditsB float64
	// FillScale and HiddenUSDPerHour calibrate the allowance view. A wrong
	// capacity table shows up as a scale on the fill; usage the same seat burns
	// where Terma cannot see it (claude.ai chat, another machine) shows up as a
	// steady drip into the windows. FitDays is how many reconciled user-days
	// the fit rests on.
	FillScale        float64
	HiddenUSDPerHour float64
	FitDays          float64
}

func (p *Prior) scale() float64 {
	if p == nil || p.FitDays == 0 {
		return 1
	}
	return p.FillScale
}

func (p *Prior) hiddenRate() float64 {
	if p == nil || p.FitDays == 0 {
		return 0
	}
	return p.HiddenUSDPerHour
}

// pOver is the probability that a call at this fill ran over a limit. The
// logistic width reflects that the view is built from one machine's calls
// against a guessed capacity.
func pOver(fill, scale float64) float64 {
	return 1 / (1 + math.Exp(-(scale*fill-1)/fillSteepness))
}

func (p *Prior) routeObs(r Route) float64 {
	if p == nil || p.RouteObs == nil {
		return 0
	}
	return p.RouteObs[r]
}

// addRoute records route evidence. Total evidence is capped so that a change
// of configuration (a /login, an approved key) can overturn old days.
func (p *Prior) addRoute(r Route, n float64) {
	if n <= 0 {
		return
	}
	if p.RouteObs == nil {
		p.RouteObs = map[Route]float64{}
	}
	p.RouteObs[r] += n
	var total float64
	for _, v := range p.RouteObs {
		total += v
	}
	if total > routeMemory {
		for k := range p.RouteObs {
			p.RouteObs[k] *= routeMemory / total
		}
	}
}

// addCreditShare records one reconciled user-day's credit share, capped the
// same way so that habits can change.
func (p *Prior) addCreditShare(f float64) {
	p.CreditsA += f
	p.CreditsB += 1 - f
	if total := p.CreditsA + p.CreditsB; total > creditMemory {
		p.CreditsA *= creditMemory / total
		p.CreditsB *= creditMemory / total
	}
}

// creditObs is how many reconciled user-days the credit prior rests on.
func (p *Prior) creditObs() float64 { return p.CreditsA + p.CreditsB }

// Config is the estimator's world knowledge.
type Config struct {
	// Allowance is the believed included capacity per rolling window, in
	// reference USD, keyed by Claude seat tier or Codex plan type. It does not
	// have to be right: the prior learned from reports corrects a wrong table.
	Allowance map[string][]Window
	// Default applies to tiers the table does not know and to sessions with no
	// account snapshot.
	Default []Window
}

// Estimator turns evidence into estimates and carries per-user state: the
// learned priors and a rolling view of each user's observed spend.
type Estimator struct {
	cfg    Config
	priors map[string]*Prior
	usage  map[string]*Allowance
	lastAt map[string]time.Time
}

// Tunables. Each is a statement about how far to trust a kind of evidence.
const (
	// pRule is the confidence in a documented provider rule: fast mode on a
	// subscription draws on credits; Codex's auth_mode names its credential.
	pRule = 0.98
	// pState is the confidence in a state read off the harness's own config
	// when that state selects a route by documented precedence.
	pState = 0.97
	// pPlain is the confidence that a machine with a login profile and no
	// competing credential configured is on that login. Every residual here is
	// shown as expected spend on accounts that cannot spend, so it is small.
	pPlain = 0.99
	// defaultKeyApproved is the assumed share of interactive users who approved
	// an environment API key when Claude Code asked. The docs say the key is used
	// once approved; most people approve.
	defaultKeyApproved = 0.75
	// routePriorWeight is how many learned observations the route defaults are
	// worth; routeMemory caps the learned total.
	routePriorWeight = 4.0
	routeMemory      = 12.0
	// creditPriorA/B is the default belief about the credit share of normal-speed
	// subscription spend when credits are available: Beta(0.5, 2), mean 0.2,
	// worth two and a half reconciled days. creditMemory caps the learned total.
	creditPriorA, creditPriorB = 0.5, 2.0
	creditMemory               = 20.0
	// flatShareWeight is how much the learned flat credit share counts against
	// the allowance view while there are too few reconciled days to calibrate it.
	flatShareWeight = 0.3
	// fillSteepness widens the transition from included to credits around a
	// full window, because the estimator's view of the window is incomplete.
	fillSteepness = 0.12
	// limitMemory bounds the effect of a confirmed, scoped budget block
	// unless evidence explicitly clears that block sooner.
	limitMemory = 5 * time.Hour
	// quotaFreshness is how old a status-line snapshot may be and still speak
	// for a call; quotaSteepness is the logistic width around a full window
	// for the provider's own figure, much sharper than for our reconstruction.
	quotaFreshness = 15 * time.Minute
	quotaSteepness = 0.04
	// quotaWeight is how much the provider's snapshot counts against our view.
	quotaWeight = 0.85
)

// New returns an estimator with nothing learned.
func New(cfg Config) *Estimator {
	return &Estimator{cfg: cfg, priors: map[string]*Prior{}, usage: map[string]*Allowance{}, lastAt: map[string]time.Time{}}
}

// Priors exposes the learned state so it can be carried into a fresh estimator.
func (e *Estimator) Priors() map[string]*Prior { return e.priors }

// SetPriors replaces the learned state with a copy.
func (e *Estimator) SetPriors(p map[string]*Prior) {
	e.priors = map[string]*Prior{}
	for k, v := range p {
		cp := *v
		cp.RouteObs = map[Route]float64{}
		for r, n := range v.RouteObs {
			cp.RouteObs[r] = n
		}
		e.priors[k] = &cp
	}
}

func (e *Estimator) prior(user string) *Prior {
	p, ok := e.priors[user]
	if !ok {
		p = &Prior{}
		e.priors[user] = p
	}
	return p
}

// windowsFor is the believed capacity for a tier.
func (e *Estimator) windowsFor(tier string) []Window {
	if w, ok := e.cfg.Allowance[tier]; ok {
		return w
	}
	return e.cfg.Default
}

func (e *Estimator) tracker(user, tier string) *Allowance {
	key := user + "\x00" + tier
	t, ok := e.usage[key]
	if !ok {
		t = NewAllowance(e.windowsFor(tier))
		e.usage[key] = t
	}
	return t
}

// drip feeds the learned hidden usage into a user's view for the time since
// their last call, the same way the seat's other surfaces would have.
func (e *Estimator) drip(user string, tr *Allowance, at time.Time) {
	if last, ok := e.lastAt[user]; ok {
		if rate := e.prior(user).hiddenRate(); rate > 0 {
			if h := at.Sub(last).Hours(); h > 0 {
				tr.Add(at, rate*h)
			}
		}
	}
	e.lastAt[user] = at
}

// Estimate answers for one call. Calls must be fed in time order per user for
// the rolling allowance view to mean anything.
func (e *Estimator) Estimate(s Session, c Call) Estimate {
	route, learnable, routeBasis := e.routeDistribution(s, c)
	tier := s.Account.Tier()
	tr := e.tracker(s.UserID, tier)
	e.drip(s.UserID, tr, c.At)
	pCredits, basis := e.creditProbability(s, c, tr)
	pSub := route[RouteSubscription]
	p := map[Funding]float64{
		FundingIncluded: pSub * (1 - pCredits),
		FundingCredits:  pSub * pCredits,
		FundingMetered:  1 - pSub,
	}
	if pSub < 0.5 {
		basis = "route:" + routeBasis
	}
	if c.Speed != "fast" && c.ReferenceUSD > 0 {
		// Only included spend consumes the allowance; credits ride on top of it.
		// Feed the view the expected included share so that a window rolls back
		// under its limit when the truth would.
		tr.Add(c.At, c.ReferenceUSD*(1-pCredits))
	}
	return Estimate{
		CallID:             c.ID,
		P:                  p,
		PRoute:             route,
		ExpectedMeteredUSD: (p[FundingCredits] + p[FundingMetered]) * c.ReferenceUSD,
		Basis:              basis,
		RouteLearnable:     learnable,
		Tier:               tier,
	}
}

// routeDistribution is where the credential hints, Codex's auth_mode and the
// learned route history meet. Every branch is a documented Claude Code rule or
// an explicit default, so a wrong guess is traceable to one line.
func (e *Estimator) routeDistribution(s Session, c Call) (dist map[Route]float64, learnable bool, basis string) {
	pr := e.prior(s.UserID)
	if c.Harness == HarnessCodex {
		// The docs spell the credential classes "swic" and "api"; Codex 0.154.0
		// emits "Chatgpt" (live, 2026-09-15). Match the meaning, not the spelling.
		switch strings.ToLower(c.AuthMode) {
		case "api", "apikey", "api_key":
			return spread(RouteAPIKey, pRule), false, "auth_mode"
		case "swic", "chatgpt":
			return spread(RouteSubscription, pRule), false, "auth_mode"
		}
		return e.learnedRoute(pr, map[Route]float64{RouteSubscription: 0.7, RouteAPIKey: 0.3}), true, "default"
	}
	h := s.Hints
	switch {
	case h.CloudProvider:
		return spread(RouteCloud, pState), false, "hint"
	case h.AuthToken:
		return spread(RouteGateway, pState), false, "hint"
	case h.APIKey && c.Entrypoint != "" && c.Entrypoint != "cli":
		// Non-interactive mode always uses an environment key when one is set.
		return spread(RouteAPIKey, pState), false, "hint"
	case h.APIKey:
		// Interactive: the key is used once approved, and the approval is a
		// remembered per-user choice we cannot see. Learn it.
		return e.learnedRoute(pr, map[Route]float64{RouteAPIKey: defaultKeyApproved, RouteSubscription: 1 - defaultKeyApproved}), true, "prior"
	case h.APIKeyHelper:
		return spread(RouteAPIKey, 0.9), false, "hint"
	case s.Account != nil || h.OAuthTokenEnv:
		return spread(RouteSubscription, pPlain), false, "login"
	}
	return e.learnedRoute(pr, map[Route]float64{RouteSubscription: 0.6, RouteAPIKey: 0.4}), true, "default"
}

// spread gives one route p and shares the remainder equally among the others.
func spread(r Route, p float64) map[Route]float64 {
	out := map[Route]float64{}
	rest := (1 - p) / float64(len(Routes)-1)
	for _, x := range Routes {
		out[x] = rest
	}
	out[r] = p
	return out
}

// learnedRoute blends a default over the candidate routes with the learned
// route observations, Dirichlet-style: defaults count routePriorWeight days.
func (e *Estimator) learnedRoute(pr *Prior, base map[Route]float64) map[Route]float64 {
	out := map[Route]float64{}
	var total float64
	for _, r := range Routes {
		b, candidate := base[r]
		if !candidate {
			out[r] = 0.005
		} else {
			out[r] = b*routePriorWeight + pr.routeObs(r)
		}
		total += out[r]
	}
	for r := range out {
		out[r] /= total
	}
	return out
}

// creditProbability is P(usage credits | subscription route) for this call.
func (e *Estimator) creditProbability(s Session, c Call, tr *Allowance) (float64, string) {
	if c.Speed == "fast" {
		return pRule, "provider_rule"
	}
	if available, known := s.Account.CreditAvailability(); known && !available {
		return 1 - pPlain, "account_state"
	}
	if recentLimit(s.Limits, c.At) {
		// The provider refused a request: credits are not flowing for this user.
		return 0.05, "limit_event"
	}
	pr := e.prior(s.UserID)
	priorMean := (pr.CreditsA + creditPriorA) / (pr.creditObs() + creditPriorA + creditPriorB)
	pAllow := priorMean
	if len(e.windowsFor(s.Account.Tier())) > 0 {
		pAllow = pOver(tr.Fill(c.At, c.ReferenceUSD), pr.scale())
	}
	if q := c.Quota; q != nil && q.Fill() >= 0 && c.At.Sub(q.At) <= quotaFreshness && !c.At.Before(q.At) {
		// The provider's own allowance figure can reach or exceed 100%. Below
		// that, closeness says how likely this call tips over. Organization
		// spending-cap exhaustion needs separate evidence and scope.
		pq := 1 / (1 + math.Exp(-(q.Fill()-1)/quotaSteepness))
		if q.Fill() >= 0.999 {
			pq = pRule
		}
		return clamp(quotaWeight*pq+(1-quotaWeight)*pAllow, 0.01, 0.99), "quota"
	}
	// Production replay must not silently use the simulator's illustrative
	// allowance capacities. Without a table or a fresh quota, use a weak prior.
	if len(e.windowsFor(s.Account.Tier())) == 0 {
		return priorMean, "prior"
	}
	n := pr.creditObs()
	// The allowance view carries the timing of the day and, once calibrated,
	// the user's hidden usage and the table's error. The flat credit share is
	// only a bridge for the days before reconciliation has enough to fit on: a
	// week-one share is a poor guide to week two, when the weekly window bites.
	w := 1.0
	if n > 0 && pr.FitDays == 0 {
		w = 1 - flatShareWeight
	}
	if s.Account == nil {
		// No profile on disk: the tier is a guess, so lean on what was learned.
		w = 0.5
	}
	p := w*pAllow + (1-w)*priorMean
	basis := "inference"
	if pr.FitDays > 0 {
		basis = "calibrated"
	} else if n > 0 {
		basis = "prior"
	}
	return clamp(p, 0.01, 0.99), basis
}

func recentLimit(limits []LimitEvent, at time.Time) bool {
	for _, l := range limits {
		budget := l.Scope == "organization_budget" || l.Scope == "user_budget"
		cleared := !l.ClearedAt.IsZero() && !l.ClearedAt.Before(l.At) && !l.ClearedAt.After(at)
		if budget && !cleared && (l.Kind == "rate_limit" || l.Kind == "billing_error") && !l.At.After(at) && at.Sub(l.At) < limitMemory {
			return true
		}
	}
	return false
}

func clamp(x, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, x)) }
