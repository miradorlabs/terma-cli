# funding-model

Who paid for a model call, and how far off would Terma be if it said so?

Terma shows `cost_usd` today: the harness's list-price estimate. On a seat-based
plan most of that money never left the account. This module is the estimator that
says which bucket paid (seat allowance, usage credits, API metering) and how much
was actually metered, plus a harness that runs it over simulated organisations
with known truth and measures the gap to what the provider would have charged.

Route and funding are inferred from incomplete evidence. The simulator uses
user × model × day USD reports to test learning. The inspected real exports have
different grain: Claude account/model/selected-period USD and OpenAI workspace/
product/interval credits. The [local reconciliation pilot](replay/README.md)
preserves those scopes and units. Simulation accuracy is not real billing accuracy.

## Run

Docker only; the host needs no Go.

```sh
make eval                       # every scenario, 20 seeds, exit 1 if a case is missed
make eval SCENARIO=mixed-org SEEDS=50
make test                       # unit tests + the same cases
docker run --rm terma-funding-model -scenario hidden-surface -per-seed
docker run --rm terma-funding-model -scenario mixed-org -breakdown 3   # per user, why
```

## Local export comparison

`cmd/funding-reconcile` reads explicit report scope, provider CSV and normalized
settled calls with production funding events. It reports estimated usage-credit
USD, unmatched coverage, missing prices and raw Codex credits until an explicit
valuation is supplied. It uses no illustrative price/capacity tables and does not
learn from the period being measured. See [input contract and commands](replay/README.md).
Real-period reconciliation still needs matching private exports and telemetry.

## What the simulation table means

```
Δ = |shown − charged| / total reference cost
```

| Column | Meaning |
|---|---|
| naive Δ | today's behaviour: everything at list price |
| cold Δ | the estimator with nothing learned, over the scored period |
| acc | share of calls whose most likely funding is the true one |
| brier | calibration of P(metered); 0 is perfect |
| alloc err | after reconciling, how far export spend allocated to calls lands from the truth per call |
| warm Δ 2nd | second half of the period, after learning from the first half's exports |
| cold Δ 2nd | the same calls with nothing learned |
| (p90) | 90th percentile over seeds; the envelope the cases bound |

Every scenario runs 21 days: a warm-up week the estimator sees but nobody scores
(so windows do not start empty), a week that is reconciled and learned from, and a
week that judges the learning.

## Where the evidence comes from

Every field the estimator reads maps to one mechanism Terma already has or can
add as a thin hook. Nothing here needs a resident process, a provider endpoint
or a credential read.

| Evidence | Mechanism | Status |
|---|---|---|
| per-call model, tokens, `cost_usd`, `speed`, `prompt.id`, `request_id`, `app.entrypoint` | Claude Code OTel `api_request`, already stored in `ai_events` | in production |
| per-call tokens, model, `auth_mode`, `user.account_id` | Codex OTel, already stored | in production |
| window fill (`rate_limits.five_hour` / `seven_day` used %), `fast_mode`, `prompt_id` | Claude Code status line: `terma hook statusline` spools `terma.session.quota` and passes the payload through to whatever renderer was configured (`internal/hookrun/statusline.go`) | built; **live-verified on a Team seat 2026-09-15**, not only Pro/Max as documented |
| account snapshot: `billingType`, `organizationType`, `seatTier`, `hasExtraUsageEnabled`, `cachedExtraUsageDisabledReason` | `SessionStart` / `Stop` hooks reading `oauthAccount` in `~/.claude.json` | built on branch; backend mapping pending |
| credential hints: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, cloud flags | the hook's environment (live-verified: hooks see these two by name; `CLAUDE_CODE_OAUTH_TOKEN` is stripped); `apiKeyHelper` from settings | built on branch; backend mapping pending |
| limit events | a `StopFailure` hook | built on branch; backend mapping pending |
| Codex `plan_type`, credits | Codex `Stop` hook reading the thread's rollout tail | built on branch; backend mapping pending |
| ground truth | Team spend CSV, OpenAI credits CSV; later Enterprise Analytics and Costs APIs | manual upload first |

The collection contract is documented in [FUNDING-INSTRUMENTATION.md](../../docs/FUNDING-INSTRUMENTATION.md).
Collectors are implemented; wiring their events into this estimator remains backend work.

The status line is the strongest per-turn signal: it is the provider's own view
of the windows, so it already includes usage on other surfaces and devices.

## Current envelope (20 seeds)

| scenario | naive | cold (p90) | warm 2nd (p90) | cold 2nd | what it isolates |
|---|---|---|---|---|---|
| team-allowance-only | 100% | 2.0% (2.0%) | 2.0% (2.0%) | 2.0% | this machine: credits disabled at org level, nothing can be metered |
| team-credits | 34% | 1.7% (2.9%) | 1.7% (4.6%) | 1.9% | credits on, heavy users cross the 5h and 7d windows, status line present |
| team-credits-wrong-table | 34% | 5.9% (7.5%) | 2.9% (7.1%) | 5.9% | estimator's capacity table 40% too generous |
| team-credits-sparse-export | 29% | 4.6% (8.4%) | 4.9% (7.8%) | 4.8% | export lists only days with credit spend |
| fast-mode | 53% | 0.1% (0.3%) | 0.2% (0.4%) | 0.2% | documented rule: fast on a subscription is credits |
| api-key-mix | 55% | 14.9% (18.8%) | 4.3% (6.0%) | 15.3% | environment key approved by some, declined by others |
| login-switch | 53% | 9.1% (11.1%) | 9.3% (13.4%) | 12.0% | /login from Team seat to Console mid-session, snapshot re-read per turn |
| login-switch-stale-snapshot | 53% | 9.7% (11.2%) | 7.4% (11.4%) | 12.0% | same, snapshot only at SessionStart |
| hidden-surface | 39% | 7.8% (9.8%) | 2.0% (4.2%) | 7.7% | the seat burns allowance in claude.ai; the status line reports it anyway |
| pro-max-quota | 38% | 3.3% (4.9%) | 3.4% (5.3%) | 3.4% | individual plans: status line, no organisation export to learn from |
| codex-business | 57% | 1.4% (2.4%) | 2.2% (4.1%) | 2.2% | auth_mode names the route; credits export reconciles |
| mixed-org | 41% | 1.0% (2.4%) | 1.2% (2.3%) | 1.4% | all of the above in one organisation, plus a gateway user |
| team-credits-no-statusline | 34% | 4.0% (5.9%) | 4.5% (10.5%) | 3.4% | the allowance view alone, no status line |
| hidden-surface-no-statusline | 39% | 51.2% (55.3%) | 6.2% (11.3%) | 52.2% | the blind spot without a status line; reconciliation learns a drip |

The cases in `eval/cases.go` bound these numbers. They describe the current
envelope, not a proof: a case failing means a change made the estimate worse on
the one thing that scenario isolates. The two `no-statusline` rows are the
price of not having the status line, as a number.

## Layout

- `model/` is the estimator and is meant to move to the backend unchanged. It
  has no I/O and knows nothing about the simulator.
  - `Estimate(session, call)` returns a distribution over funding, a
    distribution over route, the expected metered USD and the strongest kind of
    evidence behind it (`provider_rule`, `account_state`, `limit_event`,
    `calibrated`, `prior`, `inference`, `route:*`).
  - Route evidence: Claude Code's credential hints under its documented
    precedence, Codex's `auth_mode`, and a per-user prior learned from which
    export a user shows up in. Only calls whose route came from a learnable
    default teach that prior.
  - Credits evidence: fast mode by rule; the account snapshot's credit policy;
    a `StopFailure` rate limit; the status line's own window fill when a fresh
    snapshot exists; otherwise a rolling view of the user's own spend against
    the plan's windows, calibrated per user by reconciliation
    with a fill scale (for a wrong capacity table) and a hidden USD-per-hour
    drip (for usage on surfaces Terma cannot see). The calibration is fitted by
    replaying reconciled days on a grid and accepted only if it also improves
    held-out days.
  - `Reconcile(kind, rows, calls, estimates)` matches an export to calls at
    simulated user × model × UTC day for the users the fixture covers, allocates reported
    spend across calls in proportion to expected metered spend, and reports
    unmatched spend. `Learn(result)` updates the priors.
- `sim/` builds worlds: personas with a true route, account snapshot,
  credential hints, habits, hidden usage, whether their status line exposes
  quota, and optional mid-session switch; true allowance windows; and the
  providers' exports rendered from the truth under
  their scopes (Team spend meters credit spend only; Console usage meters API
  keys at list; the OpenAI credits export meters Codex credits; gateway and
  cloud produce no export).
- `eval/` runs a scenario over seeds, computes the metrics above and holds the
  cases. `Breakdown` explains one seed per user.

## What is deliberately not modelled yet

- Mixed funding inside one request (OpenAI describes transitioning to credits
  mid-request); a call is one bucket here.
- Declared seat fees and their allocation to sessions; this module stops at
  metered spend and leaves the subscription share to a later policy layer.
- Contracted prices (`modelPricing`), data-residency multipliers, promotional
  credits and refunds in the exports.
- Report freshness and revision; exports are taken as final for their day.
- Enterprise Analytics API and OpenAI Costs API shapes; they land in the same
  `ReportRow` when added.
