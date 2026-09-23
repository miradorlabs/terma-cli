# Local billing reconciliation pilot

Compare captured calls with provider usage-credit exports at their actual scope.
This is an offline POC command, not a production billing service or a new `terma`
subcommand. It never uploads input, changes account settings, or learns from the
period it measures.

## Try the synthetic fixtures

From `pocs/funding-model` (Docker only):

```sh
make reconcile SCOPE=replay/testdata/claude-scope.json REPORT=replay/testdata/claude.csv CAPTURE=replay/testdata/capture.json > /tmp/claude-reconciliation.json
make reconcile SCOPE=replay/testdata/openai-scope.json REPORT=replay/testdata/openai.csv CAPTURE=replay/testdata/capture.json > /tmp/codex-reconciliation.json
```

For private inputs, place copies in the gitignored `local/` directory, then use
paths under that directory. Inputs must be regular files of at most 32 MiB each.
Outputs contain account/session identifiers; keep real results in `local/` too.

## Three inputs

### Scope JSON

See [Claude](testdata/claude-scope.json) and [OpenAI](testdata/openai-scope.json).

- `organization_id` is the **provider** organization/workspace ID, not a Terma
  project or organization ID. `source` records how the export was bound to that
  organization and period. Export CSVs do not themselves establish all this scope.
- `start` and `end` are UTC `YYYY-MM-DD`; **end is exclusive**. For Claude they must
  describe the entire selected export period; the CSV has no row-level dates.
  The pilot cannot verify those dates from the CSV itself.
- `product` is an exact CSV product value for Claude; OpenAI supports `Codex`.
- Claude `measure` is `net_usd` or `gross_usd`. Choose deliberately: gross reference
  prices versus net amounts may leave a discount difference.
- OpenAI `measure` is `credits`. Dollars remain absent unless you supply a positive
  `credits_usd_per_unit` plus `valuation_source` explaining the applicable rate.
  A credit balance or top-up alone does not establish that rate. A varying rate
  needs separate comparisons for each interval with a known rate.
- `coverage` (`complete`, `partial`, `unknown`) records the provider export's
  coverage, not whether Terma captured every call. Missing rows remain unknown.

### Provider CSV

Schemas were inspected in the earlier exploration; see
[retained schema evidence](evidence/2026-09-15-provider-report-capabilities.json).

| Provider | Selected columns | Comparison grain | Unit |
|---|---|---|---|
| Claude | `account_uuid`, `product`, `model`, `total_net_spend_usd` / `total_gross_spend_usd` | Account × model × selected period | USD |
| OpenAI | `Start Time`, `End Time`, `Codex` (header also has `Work`, `Chat`) | Workspace × Codex × exported interval | Credits |

Additional columns are ignored. OpenAI date cells currently require UTC dates
with exclusive end; timestamps or different boundary semantics need an explicit
adapter. Rows crossing the chosen period boundary, overlapping aggregates,
negative adjustments and malformed amounts are rejected. Blank credit cells are
missing, explicit `0` cells are zero. Raw amount strings preserve decimal precision;
heuristic monetary calculations use floating point, not an accounting ledger.

### Capture JSON

See [synthetic capture](testdata/capture.json); the contract is in [types.go](types.go).
This is a normalized export, **not raw OTLP or the direct output of `terma session
show`**. Producing it from stored AI events remains a backend/export adapter task.

- Version `1`, same provider `organization_id`, and `source` identifying the
  telemetry export and its coverage. Do not combine different provider orgs.
- Each call is one **settled native usage record** with a stable, globally unique
  `id`, session, user, harness, product, model, and `at` timestamp. Use the request's
  start time when available: this selects pre-call evidence. If only completion
  time is available, identify that limitation in the source and exclude any
  snapshot whose relative order cannot be established.
- `account_id` is the provider identity. Claude needs it to join `account_uuid`;
  email/user IDs are not guessed into account IDs. Model names likewise require
  an explicit upstream mapping if the report and telemetry use different names.
- `reference_usd` is a per-call reference cost from native telemetry or a backend
  calculation with a verified, dated rate. Set it to `null` if unavailable; zero
  means a known zero. A numeric value requires `price_source`. Never use cumulative
  session counters as per-call spend, or this module's illustrative price table.
- Optional per-call `auth_mode`, `speed` and `entrypoint` retain native evidence.
  Keep only settled usage records; failures do not create invented calls or cost.
- `events` use the production spool shape: `time`, `name`, `session_id`, optional
  `repo`, and `attrs`. Delivered OTLP attributes must first be flattened; booleans
  and numbers accept their native or string forms. No credentials are needed.
- Exact repeated call IDs are deduplicated. Conflicting duplicates fail. For an
  API-only route, call estimates remain useful but these seat exports cannot
  reconcile its API charges.

## Evidence and output

Equal observation timestamps are ordered by `source_offset` (Codex) or
`observation_sequence` (Claude) within the same `source_stream`. Transport retries
may repeat observation IDs; native call deduplication remains separate.

The pilot joins account/quota evidence by harness and session, never from after
`at`. Claude account state additionally requires matching provider account and org.
Account state expires after 30 minutes, allowance snapshots after 15 minutes or
at the window reset. These are conservative pilot cutoffs, not provider guarantees.
A new account observation invalidates an older quota; missing/unavailable evidence
supersedes populated evidence. Codex uses its rollout source timestamp as well as
observation time. Workspace binding for Codex relies on the explicit capture scope.

Percentages above 100 remain usable allowance evidence. `spend_limit` is not an
allowance window or a dollar denominator. Generic StopFailure categories cannot
prove an organization budget block. The model supports scoped budget blocks with
an explicit clearance time; the live collector does not yet supply that scope or
numeric admin policy. Unknown credit availability stays unknown, not disabled.

Output contains:

- Per-call estimated funding, **uncalibrated heuristic probabilities**, evidence
  basis, warnings, and expected metered USD if priced (usage credits plus API).
- At each report row's scope, `estimated_usage_credit_usd` is the sum of reference
  cost × probability of usage-credit funding. It excludes API funding and seat fees.
- `estimated_minus_reported_usd` only when matched calls are all priced and the
  report has a USD amount or explicit valuation. It is a comparison of observed
  usage against the reported total, not a measured model accuracy score.
- `unmatched_report`, `unpriced_calls` and `unvalued_credits` retain gaps instead of
  forcing the bill onto captured calls. A `compared` status is not a claim of full
  telemetry coverage. Other-device activity, discounts and export lag can remain
  in the difference. No percentage accuracy is claimed from partial coverage.

Next: run on matching real files, explain coverage and price differences, then
calibrate on an earlier period and evaluate on a separate later period. Production
backend mapping, API billing imports, admin credit/cap collection and separate seat
cost allocation remain outstanding.
