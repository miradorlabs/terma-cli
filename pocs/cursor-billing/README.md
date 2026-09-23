# Cursor billing importer

A standalone Go package for a platform worker. It is a separate module so it can
move into the backend without importing Terma's CLI, hooks or local config.
The package uses only the standard library. `cmd/cursor-billing-import` is a local
validation adapter; it is not a `terma` command or part of its release binary.

## Platform integration

The platform owns the team API key, tenant authorization, secret storage, worker
schedule and database. Pass the decrypted key directly to the client; the library
never loads environment variables, reads credentials from disk or persists data.

```go
import billing "github.com/miradorlabs/terma-cli/pocs/cursor-billing"

// Inside a platform job, after authorizing the stored integration connection:
client, err := billing.New(billing.Config{
    APIKey: teamKeyFromSecretStore,
    Scope: billing.Scope{TenantID: tenantID, ConnectionID: connectionID},
    HTTPClient: workerHTTPClient, // optional; redirects are always disabled
})
if err != nil { return err }

members, err := client.Members(ctx)
if err != nil { return err }
spend, err := client.Spend(ctx)
if err != nil { return err }
usage, err := client.Usage(ctx, billing.Window{Start: startUTC, End: endUTC})
if err != nil { return err }
joined, err := billing.JoinSessions(usage, usage.Scope, knownHookSessions)
if err != nil { return err }
// Persist members/spend as observed snapshots; atomically replace usage for the
// exact scope/window. Persist joined costs as derived data for that revision.
```

This module can be copied into the platform or consumed as a Go module once this
branch is published; use a local Go `replace` while developing across checkouts.
No platform database or worker has been changed in this repository.

### Persistence and scheduling contract

1. Bind the key to an authorized tenant connection in the platform. These endpoints
   do not return a verified team ID. Keep the connection ID across same-team key
   rotation; create a new connection when switching teams. Do not let tenants
   choose `BaseURL`; that option is for trusted operators/test servers.
2. Partition usage into fixed, nonoverlapping windows, for example UTC days.
   Windows are half-open `[start, end)` with millisecond precision, up to 31 days.
   The API's inclusive end is translated to `end - 1ms`.
3. Serialize imports per connection/window (or use a fencing token). In one database
   transaction, replace **all** rows for `(tenant, connection, start, end)` only
   after a successful complete import. Replace derived joins in that transaction
   too. Commit the checkpoint after the transaction. On any failure, retain the
   previous snapshot/checkpoint. Empty complete windows can replace old rows.
4. Store the snapshot `ContentID` and an import revision. A repeated content ID
   can skip rewriting data. Store row ordinal within the revision: fingerprints
   are **not unique event IDs**. Identical rows retain their multiplicity.
5. Refresh recent fixed windows to capture delayed records/corrections. Cursor
   documents hourly aggregation and recommends polling no more than hourly.
   A lookback is an operational choice, not a guarantee that older billing cannot
   change; reconcile the full billing period separately. Refresh windows by
   replacement; never sum overlapping polls or append each poll as new usage.
6. A page failure, changed pagination metadata, ambiguous repeated page, bound
   violation or schema error returns no snapshot. `HTTPError` exposes status and
   `RetryAfter`; the platform schedules retries/backoff for 429/5xx and alerts on
   invalid credentials. The library performs no automatic retries. Split windows
   on record limits; use smaller pages on response-size limits.

Defaults: 1,000 rows/page, 1,000 pages, 100,000 records, 8 MiB/response, 30 seconds
per HTTP request. Pass an overall context deadline. The provider has no snapshot
isolation: unchanged counts cannot detect every concurrent update. `Complete`
means traversal completed, not that the provider has reported every request or
that the bill is final. Members, spend and usage are independently observed and
are not an atomic provider snapshot.

## Meaning of the data

| Field | Treatment |
|---|---|
| `tokenUsage.totalCents` | Provider model reference value, in fractional cents |
| `chargedCents` | Provider-reported charge; retain `kind` and `isChargeable` alongside it. Included usage may have nonzero charge, so this is not automatically extra cash |
| `cursorTokenFee`, `requestsCosts` | Preserve raw amounts; do not add them again to a charge without verifying its composition |
| `spendCents` | Member on-demand spend snapshot for the current subscription cycle |
| `includedSpendCents`, `overallSpendCents` | Preserve independently when supplied; do not synthesize one from the other |
| `billingTier` | Raw provider code; no guessed plan or seat mapping |
| `apiPercentUsed`, `autoPercentUsed`, `totalPercentUsed` | Raw percentages, including values above 100; denominators/seat mapping await dashboard comparison |
| `subscriptionCycleStart` | Observed cycle start, not a computed next reset time |
| `monthlyLimitDollars`, `hardLimitOverrideDollars`, `effectivePerUserLimitDollars` | Raw dollar-valued policy fields; null/absent is unknown |

Decimal values serialize as strings and are added exactly, without binary-float
rounding. Units stay in field names: cents and dollars are not interchangeable.
Negative financial adjustments are retained. Missing tokens/prices stay missing;
explicit zero stays zero. Original row JSON preserves extra fields and absent/null
information. Spend retains full response pages too. These records contain personal
and financial data; keep them within the platform's authorized integration store.
Errors omit response bodies and credentials; do not log client/config structs or
private snapshots.

`JoinSessions` requires matching tenant/connection scope and exact conversation
IDs. Supply the hook account email when known to reject identity mismatches.
The result keeps model value, reported charge, missing-price counts, raw billing
category counts and unmatched records separate. Session turn IDs are descriptive;
no cost is allocated between turns without an authoritative request/generation
join. No matched record means `not_observed`, not zero cost. Joining one window
only covers that window's portion of a conversation.

## Local verification

```sh
make -C pocs/cursor-billing check build

# Team key already exported as CURSOR_ADMIN_API_KEY. Output must not exist.
pocs/cursor-billing/bin/cursor-billing-import \
  -tenant local-validation -connection cursor-team \
  -out pocs/cursor-billing/local/import.json
```

The adapter alone reads `CURSOR_ADMIN_API_KEY`. It writes a new mode-0600 file and
prints only counts/path. `local/`, `bin/`, `live/report/` and local credentials are
ignored. Create `local/` first. `-start` / `-end` accept RFC3339 timestamps; defaults
are current cycle start through now. `-page-size 2` exercises pagination on a small
team. The default window is useful for manual verification; scheduled platform
imports must use fixed windows.

To capture two authenticated Cursor turns, use the **personal CLI key** in
`live/.env.local` as `CURSOR_API_KEY` (distinct from the team admin key):

```sh
TERMA_LIVE_CURSOR_CAPTURE=report/cursor-billing-validation/session.json \
  make -C live live RUN='^TestCursorBillingSession$'

# After billing data has arrived, import and join the private capture:
pocs/cursor-billing/bin/cursor-billing-import \
  -tenant local-validation -connection cursor-team \
  -capture live/report/cursor-billing-validation/session.json \
  -out pocs/cursor-billing/local/joined.json
```

The capture includes conversation/account identity and delivered hook observations;
synthetic CLI responses are saved beside it for diagnosis. A failed test cannot
be used as a passing capture. `TestCursorBillingSession` validates both edits,
resume identity and delivered conversation identity. The separate strict
`TestCursorCLITurnObservations` checks two Stop generations, tokens and commit
attribution; session-level success does not imply those checks pass.

Tests cover pagination failures, changing counts/cycles, repeated pages, inclusive
boundaries, precision, missing versus zero, identical-row multiplicity,
corrections, tenant/account mismatches, bounds, cancellation, auth/rate errors and
credential redirects. CI runs vet and race tests for this module separately.

Sources: [Cursor Admin API](https://cursor.com/docs/account/teams/admin-api),
[team dashboard](https://cursor.com/docs/account/teams/dashboard),
[Enterprise OTLP contract](https://cursor.com/docs/enterprise/opentelemetry-export/wire).
Observed account validation and remaining gaps are recorded in
[the Cursor instrumentation notes](../../docs/CURSOR-INSTRUMENTATION.md).
