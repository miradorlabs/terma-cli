// Package model is the funding estimator: given what Terma can observe about a
// model call and the session it ran in, say who most likely paid for it and how
// much left the customer's account. It is deliberately free of I/O and of any
// simulation knowledge so that it can move to the backend unchanged.
//
// Route and funding are estimated from incomplete evidence. The simulator
// renders user/model/day USD reports for learning experiments. Actual exports
// have different grain and units; package replay preserves those instead of
// feeding them into the simulation reconciliation contract.
package model

import "time"

// Harness is the coding agent that produced the call.
type Harness string

const (
	HarnessClaude Harness = "claude-code"
	HarnessCodex  Harness = "codex"
)

// Route is who the provider billed: a seat's subscription, a Console/API key, an
// LLM gateway token, or a cloud provider account.
type Route string

const (
	RouteSubscription Route = "subscription"
	RouteAPIKey       Route = "api_key"
	RouteGateway      Route = "gateway"
	RouteCloud        Route = "cloud"
)

// Routes lists every route in a stable order.
var Routes = []Route{RouteSubscription, RouteAPIKey, RouteGateway, RouteCloud}

// Funding is which bucket paid: the seat's included allowance, usage credits on
// top of a subscription, or per-token metering outside a subscription.
type Funding string

const (
	FundingIncluded Funding = "included"
	FundingCredits  Funding = "usage_credits"
	FundingMetered  Funding = "api_metered"
)

// Fundings lists every funding in a stable order.
var Fundings = []Funding{FundingIncluded, FundingCredits, FundingMetered}

// Metered reports whether the funding moved money.
func (f Funding) Metered() bool { return f != FundingIncluded }

// Tokens are the four billed buckets. Input excludes the cache buckets.
type Tokens struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Add folds another count into t.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
}

// Call is one settled model call as the pipeline sees it: the OTel api_request
// (Claude) or response.completed (Codex) after the gateway's normalization.
type Call struct {
	ID        string
	UserID    string
	SessionID string
	Harness   Harness
	At        time.Time
	Model     string
	Tokens    Tokens
	// ReferenceUSD is the harness's own list-price estimate: Claude's cost_usd, or
	// the price-table figure the backend derives for Codex. It is what Terma shows
	// as cost today and the amount at stake when the call turns out to be metered.
	ReferenceUSD float64
	// Speed is Claude's actual request speed, "fast" or "normal". Fast requests on a
	// subscription draw on usage credits by provider rule.
	Speed string
	// Entrypoint is Claude's app.entrypoint ("cli", "sdk-cli", ...). Non-interactive
	// entrypoints always use an environment API key when one is set.
	Entrypoint string
	// AuthMode is Codex's own statement of the credential class: "swic" or "api".
	AuthMode string
	// Quota is the latest status-line rate-limit snapshot for the session taken
	// before this call, when the plan exposes one. It is the provider's own view
	// of the windows, so it already includes usage Terma cannot see.
	Quota *QuotaSnapshot
}

// QuotaSnapshot is what Claude Code's status line reports under rate_limits:
// per-window used percentage and reset time. Preserve values above 100 (107%
// observed live). These allowance windows do not identify an organization USD cap.
type QuotaSnapshot struct {
	At               time.Time
	FiveHourUsedPct  *float64
	SevenDayUsedPct  *float64
	FiveHourResetsAt time.Time
	SevenDayResetsAt time.Time
}

// Fill is the fullest reported window as a ratio, or -1 when no window is present.
func (q *QuotaSnapshot) Fill() float64 {
	if q == nil {
		return -1
	}
	fill := -1.0
	for _, p := range []*float64{q.FiveHourUsedPct, q.SevenDayUsedPct} {
		if p != nil && *p/100 > fill {
			fill = *p / 100
		}
	}
	return fill
}

// AccountSnapshot is the plan state a hook reads off the harness's own files:
// Claude's oauthAccount in ~/.claude.json, or Codex's latest rate-limit record.
// Nil means no such profile exists on the machine.
type AccountSnapshot struct {
	// Claude
	BillingType              string // "stripe_subscription", ...
	OrganizationType         string // "claude_team", "claude_enterprise", "claude_max", ...
	SeatTier                 string // "team_tier_1", ...
	ExtraUsageEnabled        *bool  // nil means unknown, not disabled
	ExtraUsageDisabledReason string // "" when credits can flow; "org_level_disabled_until", ...
	// Codex
	PlanType         string // "business", "plus", "pro", ...
	HasCredits       *bool
	CreditsUnlimited *bool
}

// CreditsAvailable says whether a subscription call that runs past the allowance
// can be funded by usage credits at all. When it cannot, the harness blocks the
// call rather than charging for it.
func (a *AccountSnapshot) CreditsAvailable() bool {
	available, _ := a.CreditAvailability()
	return available
}

// Bool is convenient when building a snapshot from a known policy value.
func Bool(v bool) *bool { return &v }

// CreditAvailability preserves absent policy fields instead of treating them as
// a provider statement that credits are disabled.
func (a *AccountSnapshot) CreditAvailability() (available, known bool) {
	if a == nil {
		return false, false
	}
	if a.PlanType != "" {
		if (a.HasCredits != nil && *a.HasCredits) || (a.CreditsUnlimited != nil && *a.CreditsUnlimited) {
			return true, true
		}
		return false, a.HasCredits != nil && a.CreditsUnlimited != nil
	}
	if a.ExtraUsageDisabledReason != "" {
		return false, true
	}
	if a.ExtraUsageEnabled == nil {
		return false, false
	}
	return *a.ExtraUsageEnabled, true
}

// Tier keys the allowance table: a Claude seat tier or a Codex plan type.
func (a *AccountSnapshot) Tier() string {
	if a == nil {
		return ""
	}
	if a.PlanType != "" {
		return a.PlanType
	}
	return a.SeatTier
}

// Hints are credential sources a hook can see are configured, without reading
// any of them. They are hints because Claude Code's precedence, its one-time
// approval prompt and /login decide what is actually used.
type Hints struct {
	APIKey        bool // ANTHROPIC_API_KEY is set
	AuthToken     bool // ANTHROPIC_AUTH_TOKEN is set (gateway bearer)
	APIKeyHelper  bool // settings apiKeyHelper is configured
	CloudProvider bool // CLAUDE_CODE_USE_BEDROCK / VERTEX / FOUNDRY
	OAuthTokenEnv bool // CLAUDE_CODE_OAUTH_TOKEN is set
}

// LimitEvent is a StopFailure the hook saw: the turn ended on a provider limit.
type LimitEvent struct {
	At    time.Time
	Kind  string // "rate_limit" | "billing_error"
	Scope string // organization_budget, user_budget, allowance, or unknown
	// ClearedAt requires evidence that this particular block was lifted. A
	// generic successful call or a quota percentage drop is insufficient.
	ClearedAt time.Time
}

// Session is the evidence that arrives once per session (or once per turn, when
// the hook re-reads the snapshot at Stop) rather than once per call.
type Session struct {
	ID        string
	UserID    string
	Harness   Harness
	StartedAt time.Time
	Account   *AccountSnapshot
	Hints     Hints
	Limits    []LimitEvent
}

// Estimate is the estimator's answer for one call: a distribution over funding,
// the expected metered amount, and the strongest kind of evidence behind it.
type Estimate struct {
	CallID string
	P      map[Funding]float64
	PRoute map[Route]float64
	// ExpectedMeteredUSD is the amount expected to leave the customer's account:
	// P(metered) × the call's reference cost.
	ExpectedMeteredUSD float64
	// Basis names the strongest evidence used: provider_rule, account_state,
	// limit_event, prior, inference.
	Basis string
	// RouteLearnable marks a call whose route came from a learnable default
	// rather than a documented rule: an interactive session with an environment
	// key, or no hint at all. Only such calls teach the route prior, so days on
	// a well-understood configuration cannot bias an ambiguous one.
	RouteLearnable bool
	// Tier is the allowance table key the estimator used for this call, so a
	// reconciliation can replay the day through the same windows.
	Tier string
}

// Best is the most probable funding.
func (e Estimate) Best() Funding {
	best, bp := FundingIncluded, -1.0
	for _, f := range Fundings {
		if p := e.P[f]; p > bp {
			best, bp = f, p
		}
	}
	return best
}

// PMetered is the probability the call moved money.
func (e Estimate) PMetered() float64 { return e.P[FundingCredits] + e.P[FundingMetered] }

// PSubscription is the probability the call ran on a subscription route.
func (e Estimate) PSubscription() float64 { return e.PRoute[RouteSubscription] }

// ReportKind identifies a provider export. Each has its own scope: the Claude
// Team spend report meters usage-credit spend only; Console usage meters API-key
// spend at list; the OpenAI Business credits export meters Codex credit spend.
type ReportKind string

const (
	ReportClaudeTeamSpend    ReportKind = "claude_team_spend"
	ReportClaudeConsoleUsage ReportKind = "claude_console_usage"
	ReportOpenAICredits      ReportKind = "openai_credits"
)

// Harness says which harness a report kind describes.
func (k ReportKind) Harness() Harness {
	if k == ReportOpenAICredits {
		return HarnessCodex
	}
	return HarnessClaude
}

// ReportRow is the simulator's normalized user/model/day USD contract. Actual
// exports must use replay, whose rows retain their native grain and units.
type ReportRow struct {
	Kind     ReportKind
	UserID   string
	Model    string
	Day      time.Time // UTC midnight
	Requests int
	Tokens   Tokens
	SpendUSD float64 // as exported: cents
}

// Day truncates to the UTC day used by simulation reports.
func Day(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }
