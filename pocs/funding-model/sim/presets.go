package sim

import "github.com/miradorlabs/terma-cli/pocs/funding-model/model"

// True included capacity per tier, in list USD per window. Illustrative.
var trueAllowance = map[string][]model.Window{
	"team_tier_1": {{Length: 5 * Hour, CapacityUSD: 6}, {Length: 7 * Day, CapacityUSD: 40}},
	"team_tier_2": {{Length: 5 * Hour, CapacityUSD: 20}, {Length: 7 * Day, CapacityUSD: 140}},
	"max_5x":      {{Length: 5 * Hour, CapacityUSD: 10}, {Length: 7 * Day, CapacityUSD: 70}},
	"business":    {{Length: 5 * Hour, CapacityUSD: 8}, {Length: 7 * Day, CapacityUSD: 50}},
	"plus":        {{Length: 5 * Hour, CapacityUSD: 4}, {Length: 7 * Day, CapacityUSD: 25}},
}

var claudeModels = []string{"claude-opus-5", "claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"}
var codexModels = []string{"gpt-5.6", "gpt-5.6", "gpt-5.6-mini"}

func teamAccount(seat string, credits bool, disabled string) *model.AccountSnapshot {
	return &model.AccountSnapshot{BillingType: "stripe_subscription", OrganizationType: "claude_team",
		SeatTier: seat, ExtraUsageEnabled: model.Bool(credits), ExtraUsageDisabledReason: disabled}
}

func maxAccount(credits bool) *model.AccountSnapshot {
	return &model.AccountSnapshot{BillingType: "stripe_subscription", OrganizationType: "claude_max",
		SeatTier: "max_5x", ExtraUsageEnabled: model.Bool(credits)}
}

// claudeMax is an individual Pro/Max subscriber: the status line carries the
// provider's own window fill.
func claudeMax(id string, credits bool, sessions, calls float64) Persona {
	return Persona{ID: id, Harness: model.HarnessClaude, Route: model.RouteSubscription,
		Account: maxAccount(credits), ExposesQuota: true, SessionsPerDay: sessions, CallsPerSession: calls,
		Models: claudeModels, TokenScale: 1}
}

func codexAccount(plan string, credits bool) *model.AccountSnapshot {
	return &model.AccountSnapshot{PlanType: plan, HasCredits: model.Bool(credits), CreditsUnlimited: model.Bool(false)}
}

// claudeSub is a Team seat. Live-observed 2026-09-15 on a Team account: the
// status line carries rate_limits after the first response, so Team exposes
// quota like Pro and Max do, whatever the documentation says.
func claudeSub(id, seat string, credits bool, sessions, calls float64) Persona {
	return Persona{ID: id, Harness: model.HarnessClaude, Route: model.RouteSubscription,
		Account: teamAccount(seat, credits, ""), ExposesQuota: true, SessionsPerDay: sessions, CallsPerSession: calls,
		Models: claudeModels, TokenScale: 1}
}

// noQuota is a seat whose status line Terma does not see: no status line
// installed, hooks disabled, or a headless session.
func noQuota(p Persona) Persona { p.ExposesQuota = false; return p }

func claudeKey(id string, approved bool, sessions, calls float64) Persona {
	route := model.RouteAPIKey
	if !approved {
		route = model.RouteSubscription
	}
	return Persona{ID: id, Harness: model.HarnessClaude, Route: route,
		Account: teamAccount("team_tier_1", false, ""), Hints: model.Hints{APIKey: true},
		SessionsPerDay: sessions, CallsPerSession: calls, Models: claudeModels, TokenScale: 1}
}

func codexSub(id, plan string, credits bool, sessions, calls float64) Persona {
	return Persona{ID: id, Harness: model.HarnessCodex, Route: model.RouteSubscription,
		Account: codexAccount(plan, credits), SessionsPerDay: sessions, CallsPerSession: calls,
		Models: codexModels, TokenScale: 1}
}

func base(name string, personas ...Persona) Scenario {
	return Scenario{Name: name, Days: 21, WarmupDays: 7, Personas: personas, Allowance: trueAllowance,
		ReportIncludesAllowanceRows: true, SnapshotPerTurn: true, EstimatorAllowanceScale: 1}
}

// Presets are the cases. Each isolates one thing the estimator must get right,
// or one thing it cannot see, so a regression names its cause.
var Presets = []Scenario{
	// This machine's situation: Team seats, credits switched on but disabled at
	// the organization level. Nothing can be metered; Terma today shows list.
	base("team-allowance-only",
		func() Persona {
			p := claudeSub("ann", "team_tier_1", true, 2, 25)
			p.Account.ExtraUsageDisabledReason = "org_level_disabled_until"
			return p
		}(),
		func() Persona {
			p := claudeSub("bo", "team_tier_2", true, 3, 40)
			p.Account.ExtraUsageDisabledReason = "org_level_disabled_until"
			return p
		}(),
		func() Persona { p := claudeSub("cy", "team_tier_1", false, 1, 60); p.TokenScale = 2; return p }(),
	),
	// Credits on, a light user who never leaves the allowance and two heavy users
	// who run past their five-hour window most days.
	base("team-credits",
		claudeSub("ann", "team_tier_1", true, 2, 20),
		func() Persona { p := claudeSub("bo", "team_tier_1", true, 2, 90); p.TokenScale = 2.5; return p }(),
		func() Persona { p := claudeSub("cy", "team_tier_2", true, 3, 120); p.TokenScale = 3; return p }(),
	),
	// The same world, but the estimator's capacity table is 40% too generous.
	// The prior has to learn what the table gets wrong.
	func() Scenario {
		s := base("team-credits-wrong-table",
			claudeSub("ann", "team_tier_1", true, 2, 20),
			func() Persona { p := claudeSub("bo", "team_tier_1", true, 2, 90); p.TokenScale = 2.5; return p }(),
			func() Persona { p := claudeSub("cy", "team_tier_2", true, 3, 120); p.TokenScale = 3; return p }(),
		)
		s.EstimatorAllowanceScale = 1.4
		return s
	}(),
	// The export lists only user-days with credit spend.
	func() Scenario {
		s := base("team-credits-sparse-export",
			claudeSub("ann", "team_tier_1", true, 2, 20),
			func() Persona { p := claudeSub("bo", "team_tier_1", true, 2, 90); p.TokenScale = 2.5; return p }(),
		)
		s.ReportIncludesAllowanceRows = false
		return s
	}(),
	// Fast mode: a documented rule, so the estimator should be nearly exact.
	base("fast-mode",
		func() Persona { p := claudeSub("ann", "team_tier_2", true, 2, 30); p.FastProb = 0.6; return p }(),
		func() Persona { p := claudeSub("bo", "team_tier_1", true, 2, 30); p.FastProb = 0.2; return p }(),
		claudeSub("cy", "team_tier_1", true, 1, 20),
	),
	// Environment API keys: one developer approved the key, one declined it (and
	// so runs on the subscription), one runs the SDK where the key is always used.
	base("api-key-mix",
		claudeKey("ann", true, 2, 30),
		claudeKey("bo", false, 2, 30),
		func() Persona { p := claudeKey("cy", true, 3, 15); p.Entrypoint = "sdk-cli"; return p }(),
		claudeSub("di", "team_tier_1", false, 2, 30),
	),
	// A developer who /logins from the Team seat to a Console account midway
	// through the fourth scored day, with the hook re-reading state every turn.
	base("login-switch",
		func() Persona {
			p := claudeSub("ann", "team_tier_1", false, 2, 40)
			alt := claudeKey("ann", true, 2, 40)
			alt.Account = nil
			p.SwitchOnDay, p.Alt = 11, &alt
			return p
		}(),
		claudeSub("bo", "team_tier_1", false, 2, 30),
	),
	// The same switch seen only at SessionStart: the snapshot goes stale for the
	// rest of that session. Expected to be worse; it says what the Stop re-read buys.
	func() Scenario {
		s := base("login-switch-stale-snapshot",
			func() Persona {
				p := claudeSub("ann", "team_tier_1", false, 2, 40)
				alt := claudeKey("ann", true, 2, 40)
				alt.Account = nil
				p.SwitchOnDay, p.Alt = 11, &alt
				return p
			}(),
			claudeSub("bo", "team_tier_1", false, 2, 30),
		)
		s.SnapshotPerTurn = false
		return s
	}(),
	// The seat burns allowance in claude.ai chat where Terma cannot see it, so
	// the credits threshold arrives earlier than the observed calls suggest.
	base("hidden-surface",
		func() Persona {
			p := claudeSub("ann", "team_tier_1", true, 2, 40)
			p.OtherSurfaceUSDPerHour = 0.2
			return p
		}(),
		func() Persona {
			p := claudeSub("bo", "team_tier_1", true, 2, 40)
			p.OtherSurfaceUSDPerHour = 0.07
			return p
		}(),
	),
	// Pro and Max subscribers: the status line hands over the provider's own
	// window fill after every response, hidden usage included, so the estimator
	// should be close before any export, and there is no export to learn from.
	base("pro-max-quota",
		func() Persona {
			p := claudeMax("ann", true, 2, 60)
			p.TokenScale = 2
			p.OtherSurfaceUSDPerHour = 0.3
			return p
		}(),
		claudeMax("bo", true, 2, 30),
		func() Persona { p := claudeMax("cy", true, 3, 80); p.TokenScale = 2.5; p.FastProb = 0.2; return p }(),
		func() Persona { p := claudeMax("di", false, 2, 60); p.TokenScale = 2; return p }(),
	),
	// Codex on a Business workspace: auth_mode names the route, credits state
	// comes from the rollout, the credits export reconciles.
	base("codex-business",
		codexSub("ann", "business", true, 2, 30),
		func() Persona { p := codexSub("bo", "business", true, 3, 80); p.TokenScale = 3; return p }(),
		func() Persona { p := codexSub("cy", "business", false, 2, 60); p.TokenScale = 2; return p }(),
		func() Persona {
			p := codexSub("di", "", false, 2, 30)
			p.Route, p.Account = model.RouteAPIKey, nil
			return p
		}(),
	),
	// Everything at once, the way a real organization looks.
	base("mixed-org",
		func() Persona {
			p := claudeSub("ann", "team_tier_1", true, 2, 25)
			p.Account.ExtraUsageDisabledReason = "org_level_disabled_until"
			return p
		}(),
		func() Persona {
			p := claudeSub("bo", "team_tier_2", true, 3, 100)
			p.TokenScale = 2.5
			p.FastProb = 0.3
			return p
		}(),
		claudeKey("cy", true, 2, 30),
		claudeKey("di", false, 1, 30),
		func() Persona {
			p := claudeSub("ed", "team_tier_1", true, 2, 40)
			p.OtherSurfaceUSDPerHour = 0.15
			return p
		}(),
		codexSub("fay", "business", true, 2, 60),
		func() Persona {
			p := claudeMax("hal", true, 2, 50)
			p.TokenScale = 2
			p.OtherSurfaceUSDPerHour = 0.2
			return p
		}(),
		func() Persona {
			p := Persona{ID: "gus", Harness: model.HarnessClaude, Route: model.RouteGateway, Hints: model.Hints{AuthToken: true},
				SessionsPerDay: 2, CallsPerSession: 30, Models: claudeModels, TokenScale: 1}
			return p
		}(),
	),
}

// Preset finds a scenario by name.
func Preset(name string) (Scenario, bool) {
	for _, s := range Presets {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}
