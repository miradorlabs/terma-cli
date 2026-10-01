package model

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func teamSession(credits bool, disabled string, hints Hints) Session {
	return Session{ID: "s", UserID: "u", Harness: HarnessClaude, StartedAt: t0, Hints: hints,
		Account: &AccountSnapshot{BillingType: "stripe_subscription", OrganizationType: "claude_team",
			SeatTier: "team_tier_1", ExtraUsageEnabled: Bool(credits), ExtraUsageDisabledReason: disabled}}
}

func call(speed string, usd float64) Call {
	return Call{ID: "c", UserID: "u", SessionID: "s", Harness: HarnessClaude, At: t0, Model: "claude-opus-5",
		ReferenceUSD: usd, Speed: speed, Entrypoint: "cli"}
}

func cfg() Config {
	w := []Window{{Length: 5 * time.Hour, CapacityUSD: 6}}
	return Config{Allowance: map[string][]Window{"team_tier_1": w}, Default: w}
}

func TestFastModeOnSubscriptionIsCreditsByRule(t *testing.T) {
	e := New(cfg()).Estimate(teamSession(true, "", Hints{}), call("fast", 1))
	if e.Best() != FundingCredits || e.P[FundingCredits] < 0.9 || e.Basis != "provider_rule" {
		t.Fatalf("fast on subscription: %+v", e)
	}
}

func TestCreditsDisabledMeansIncluded(t *testing.T) {
	// This machine's state: credits on for the account, disabled by the org.
	e := New(cfg()).Estimate(teamSession(true, "org_level_disabled_until", Hints{}), call("normal", 1))
	if e.Best() != FundingIncluded || e.PMetered() > 0.1 || e.Basis != "account_state" {
		t.Fatalf("credits disabled: %+v", e)
	}
	if e.ExpectedMeteredUSD > 0.1 {
		t.Fatalf("expected near-zero metered, got %.3f", e.ExpectedMeteredUSD)
	}
}

func TestAllowanceViewRaisesCreditProbabilityWhenFull(t *testing.T) {
	est := New(cfg())
	s := teamSession(true, "", Hints{})
	var first, last Estimate
	for i := 0; i < 20; i++ {
		c := call("normal", 0.6)
		c.At = t0.Add(time.Duration(i) * time.Minute)
		e := est.Estimate(s, c)
		if i == 0 {
			first = e
		}
		last = e
	}
	if !(last.P[FundingCredits] > first.P[FundingCredits]+0.3) {
		t.Fatalf("credit probability should climb as the window fills: first %.2f last %.2f", first.P[FundingCredits], last.P[FundingCredits])
	}
}

func TestRateLimitEventMeansNoCredits(t *testing.T) {
	s := teamSession(true, "", Hints{})
	s.Limits = []LimitEvent{{At: t0.Add(-time.Minute), Kind: "rate_limit", Scope: "organization_budget"}}
	e := New(cfg()).Estimate(s, call("normal", 1))
	if e.P[FundingCredits] > 0.1 || e.Basis != "limit_event" {
		t.Fatalf("after a rate limit: %+v", e)
	}
}

func TestHintsDecideRouteWhereDocumented(t *testing.T) {
	cases := []struct {
		hints Hints
		entry string
		want  Route
		min   float64
	}{
		{Hints{CloudProvider: true}, "cli", RouteCloud, 0.9},
		{Hints{AuthToken: true, APIKey: true}, "cli", RouteGateway, 0.9},
		{Hints{APIKey: true}, "sdk-cli", RouteAPIKey, 0.9},
		{Hints{}, "cli", RouteSubscription, 0.9},
	}
	for _, tc := range cases {
		c := call("normal", 1)
		c.Entrypoint = tc.entry
		e := New(cfg()).Estimate(teamSession(false, "", tc.hints), c)
		if e.PRoute[tc.want] < tc.min {
			t.Errorf("hints %+v entry %s: want %s >= %.2f, got %+v", tc.hints, tc.entry, tc.want, tc.min, e.PRoute)
		}
	}
}

func TestInteractiveAPIKeyIsUncertainAndLearnable(t *testing.T) {
	est := New(cfg())
	s := teamSession(false, "", Hints{APIKey: true})
	cold := est.Estimate(s, call("normal", 1))
	if cold.PRoute[RouteAPIKey] < 0.5 || cold.PRoute[RouteAPIKey] > 0.9 {
		t.Fatalf("interactive key should lean api but stay uncertain: %+v", cold.PRoute)
	}
	// Five days in the Team spend report and none in Console usage: this user declined the key.
	pr := est.prior("u")
	pr.addRoute(RouteSubscription, 5)
	warm := est.Estimate(s, call("normal", 1))
	if warm.PRoute[RouteSubscription] < 0.6 || warm.Basis != "route:prior" && warm.PRoute[RouteSubscription] < 0.5 {
		t.Fatalf("learned subscription should win: %+v (%s)", warm.PRoute, warm.Basis)
	}
}

func TestCodexAuthModeIsAuthoritative(t *testing.T) {
	s := Session{ID: "s", UserID: "u", Harness: HarnessCodex, StartedAt: t0, Account: &AccountSnapshot{PlanType: "business", HasCredits: Bool(false), CreditsUnlimited: Bool(false)}}
	c := Call{ID: "c", UserID: "u", SessionID: "s", Harness: HarnessCodex, At: t0, Model: "gpt-5.6", ReferenceUSD: 1, AuthMode: "api"}
	if e := New(cfg()).Estimate(s, c); e.Best() != FundingMetered || e.P[FundingMetered] < 0.9 {
		t.Fatalf("auth_mode=api: %+v", e)
	}
	c.AuthMode = "swic"
	if e := New(cfg()).Estimate(s, c); e.Best() != FundingIncluded || e.P[FundingIncluded] < 0.9 {
		t.Fatalf("auth_mode=swic without credits: %+v", e)
	}
	// What Codex 0.154.0 actually emits for a ChatGPT login.
	c.AuthMode = "Chatgpt"
	if e := New(cfg()).Estimate(s, c); e.Best() != FundingIncluded || e.PRoute[RouteSubscription] < 0.9 {
		t.Fatalf("auth_mode=Chatgpt: %+v", e)
	}
}

func TestEstimateIsADistribution(t *testing.T) {
	e := New(cfg()).Estimate(teamSession(true, "", Hints{APIKey: true}), call("normal", 2))
	var sum float64
	for _, f := range Fundings {
		sum += e.P[f]
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("P sums to %.4f", sum)
	}
	if want := e.PMetered() * 2; e.ExpectedMeteredUSD < want-1e-9 || e.ExpectedMeteredUSD > want+1e-9 {
		t.Fatalf("expected metered %.4f, want %.4f", e.ExpectedMeteredUSD, want)
	}
}

func TestUnknownCreditPolicyIsNotDisabled(t *testing.T) {
	for _, tc := range []struct {
		name             string
		account          *AccountSnapshot
		known, available bool
	}{
		{"absent", nil, false, false},
		{"claude unknown", &AccountSnapshot{}, false, false},
		{"claude disabled", &AccountSnapshot{ExtraUsageEnabled: Bool(false)}, true, false},
		{"claude org blocked", &AccountSnapshot{ExtraUsageEnabled: Bool(true), ExtraUsageDisabledReason: "org_level_disabled_until"}, true, false},
		{"codex unknown", &AccountSnapshot{PlanType: "business"}, false, false},
		{"codex partial false", &AccountSnapshot{PlanType: "business", HasCredits: Bool(false)}, false, false},
		{"codex no credits", &AccountSnapshot{PlanType: "business", HasCredits: Bool(false), CreditsUnlimited: Bool(false)}, true, false},
		{"codex unlimited", &AccountSnapshot{PlanType: "business", CreditsUnlimited: Bool(true)}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, k := tc.account.CreditAvailability()
			if a != tc.available || k != tc.known {
				t.Fatalf("available=%v known=%v", a, k)
			}
		})
	}
	s := teamSession(true, "", Hints{})
	s.Account.ExtraUsageEnabled = nil
	unknown := New(Config{}).Estimate(s, call("normal", 1))
	s.Account.ExtraUsageEnabled = Bool(false)
	disabled := New(Config{}).Estimate(s, call("normal", 1))
	if unknown.Basis != "prior" || unknown.P[FundingCredits] < 0.15 || disabled.P[FundingCredits] > 0.02 {
		t.Fatalf("unknown %+v disabled %+v", unknown, disabled)
	}
}

func TestOnlyScopedUnclearedBudgetBlocksSuppressCredits(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		cleared     time.Time
		blocked     bool
	}{
		{"generic", "unknown", time.Time{}, false},
		{"allowance", "allowance", time.Time{}, false},
		{"org blocked", "organization_budget", time.Time{}, true},
		{"cleared", "organization_budget", t0.Add(-time.Second), false},
		{"future clearance", "organization_budget", t0.Add(time.Second), true},
		{"invalid clearance", "organization_budget", t0.Add(-2 * time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := []LimitEvent{{At: t0.Add(-time.Minute), Kind: "rate_limit", Scope: tc.scope, ClearedAt: tc.cleared}}
			if got := recentLimit(limits, t0); got != tc.blocked {
				t.Fatalf("blocked=%v", got)
			}
		})
	}
	limits := []LimitEvent{{At: t0.Add(-time.Minute), Kind: "billing_error", Scope: "organization_budget", ClearedAt: t0.Add(-time.Second)}, {At: t0.Add(-time.Minute), Kind: "billing_error", Scope: "user_budget"}}
	if !recentLimit(limits, t0) {
		t.Fatal("clearing org block cleared separate user block")
	}
}
