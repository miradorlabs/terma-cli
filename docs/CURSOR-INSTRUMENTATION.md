# Cursor collection

Cursor's IDE and `agent` / `cursor-agent` CLI share project hooks. Terma now uses
those hooks for ordered interaction evidence, alongside existing commit attribution.
Install or upgrade with `terma install --adapters cursor` in a bound repository.
Existing user hooks and their options survive installation and removal.

## What is captured

`sessionStart`, `beforeSubmitPrompt`, `afterAgentResponse`, `stop`, `preCompact`
and `sessionEnd` enqueue `terma.session.observation`. `afterFileEdit` continues to
record file attribution, now including `turn_id` when supplied. All events route
through the repository's Terma project binding and existing local spool.

| Attribute | Meaning |
|---|---|
| `session.id` (spool `session_id`) | Cursor `conversation_id`; falls back to `session_id` for older payloads |
| `turn_id` | Verbatim `generation_id`, omitted when missing; never inferred from the preceding hook |
| `provider_session_id` | Optional Cursor session identifier, separate from conversation identity |
| `hook_event` | The installed hook that received this observation |
| `model`, `model_id`, `model_param.*` | Selected model and allowlisted thinking/context/effort parameters; Auto is not a resolved billed model |
| `cursor.version` | Version supplied by Cursor |
| `account_email`, `account_status` | Hook-supplied email if available; no inference about authentication or who paid |
| `reported_input_tokens`, `reported_output_tokens`, `reported_cache_read_tokens`, `reported_cache_write_tokens` | Optional response/Stop values, preserved without adding or subtracting cache buckets |
| `usage_status` | `available` when all four fields are valid, `partial`, `unavailable`, or `invalid`; availability does not assert complete usage coverage |
| `usage_semantics`, `usage_scope` | `snapshot`, `parent_turn`; never an additive counter or a complete subagent rollup |
| `status`, `loop_count` | Stop outcome and optional continuation count; an error is not automatically a billing/limit error |
| `context_tokens`, `context_window_size`, `context_usage_percent`, `trigger` | Optional pre-compaction context occupancy, separate from usage and billing quota |
| `funding_status`, `quota_status` | `unavailable`: these hooks do not supply plan, allowance, credits or the payment bucket |
| `source_stream`, `observation_sequence`, `observation_id` | Durable per-conversation/repository ordering and replay identity |
| `ordering` | `local_receipt`; event time is Terma's receipt time, not a provider timestamp |

`subagentStop` enqueues `terma.subagent.end` under the conversation that spawned the
subagent — `parent_conversation_id` when Cursor sends one — with `agent_id` (the
subagent's own conversation), `agent_type`, `status`, `duration_ms`, `message_count`,
`tool_call_count`, `loop_count` and `file_count`. Its `modified_files` join that
conversation's manifest as `terma.files.touched` (`tool_name: subagentStop`, with the
agent and the turn). cursor-agent can also file a subagent's own `afterFileEdit` under the
subagent's conversation id; that manifest is folded into the parent's, so a commit of the
subagent's work carries one trailer, for the conversation a person can open. The task,
summary and transcript path are not read. `subagentStart` is never wired: it is a permission
gate, and a hook that prints nothing denies the spawn — which is exactly what the
committed guard does on a machine without terma. See
[Subagent collection](SUBAGENT-INSTRUMENTATION.md).

Missing/null token values remain absent; explicit zero stays zero. Negative,
fractional, nonnumeric and unsafe integer values are rejected independently, so
one bad optional counter does not erase the whole turn observation. Payloads over
4 MiB are ignored. Prompt/response text, messages, edits, error text, transcript
paths and credential values are not forwarded or read. Account email is personal
metadata and is included when Cursor supplies it.

**Do not sum response and Stop observations.** They can repeat the same snapshot.
The inspected CLI also uses cache-inclusive input counts internally; keeping the
provider fields unmodified avoids silently changing their meaning. Hook snapshots
are not sufficient for a complete model-request ledger or exact invoiced cost.

## Tool calls

`postToolUse` and `postToolUseFailure` enqueue one `terma.tool.call` per finished tool
invocation. Cursor's generic pair fires for every tool type — `Shell`, `Read`, `Write`,
`Grep`, `Delete`, `Task` and `MCP:<tool>` — and is the only hook pair that carries a
`tool_use_id` together with a duration, so it is the one wired. The event is keyed on
that id rather than on the observation checkpoint: the native id is the replay
identity, and consumers deduplicate on it.

| Attribute | Meaning |
|---|---|
| `session.id` (spool `session_id`) | Cursor `conversation_id` |
| `tool_call_id` | Verbatim `tool_use_id`; omitted when missing or not one printable token of at most 256 bytes |
| `tool_name` | Verbatim Cursor tool name (`Shell`, `Read`, `MCP:<tool>`, …); omitted when missing, multi-line or over 128 bytes |
| `hook_event` | `postToolUse` or `postToolUseFailure` |
| `status` | `completed` for `postToolUse`, `error` for `postToolUseFailure` |
| `failure_type` | `error`, `timeout`, `permission_denied`, or `unknown` for a value outside Cursor's documented set; failures only |
| `is_interrupt` | Cursor's flag that the user interrupted the call; failures only, booleans only |
| `duration_ms` | Cursor's `duration` (milliseconds), non-negative integers only; `duration_status: invalid` marks a value that was present but not one, and a missing duration stays missing |
| `turn_id`, `model`, `model_id`, `model_param.*`, `cursor.version` | As on observations |
| `ordering` | `local_receipt` |

A payload naming neither a tool nor a call id is dropped. Not read: `tool_input`,
`tool_output`, `error_message`, `agent_message`, `cwd`. The account email does not ride
on a tool call. A tool call attributes no file — `afterFileEdit` remains the manifest's
source — and starts no flush; calls ride along with the next response, Stop or session
end flush. The 4 MiB payload cap applies, so a call whose `tool_output` exceeds it is
not recorded.

Not wired, deliberately:

- `preToolUse`, `beforeShellExecution`, `beforeMCPExecution`, `beforeReadFile`: permission
  gates that run a hook process before every call and add nothing observational once
  the post hook carries the duration. Cursor fails them open only by default and only
  for crashes, timeouts and exit codes other than 2; an invalid or schema-mismatched
  reply blocks the action, `failClosed` blocks on every failure, and the reference does
  not say which of those a silent hook is. Like `subagentStart`, they never go into a
  committed file.
- `afterShellExecution`, `afterMCPExecution`: restate calls `postToolUse` already
  reported, without a `tool_use_id` and with the command output or MCP result; wiring
  them would count each shell or MCP call twice. The MCP server name they carry is the
  one thing lost — `tool_name` keeps the `MCP:<tool>` form.
- `afterAgentThought`, and `beforeReadFile`'s `content`: text.

No cross-process span tree is kept: `conversation_id` → `generation_id` →
`tool_use_id` already carries the hierarchy, so the platform parents a tool call to its
turn from the ids alone. This is the design difference from
[LangGuard's cursor-otel-hook](https://github.com/LangGuard-AI/cursor-otel-hook), a
Python hook receiver that turns every Cursor hook into a span, persists span contexts
per generation in temporary files to stitch parents across hook processes, and batches
spans per generation until `stop`. terma's spool batches and retries durably already,
and its records are logs the platform maps, not spans.

Documented contract ([hook reference](https://cursor.com/docs/agent/hooks), read
2026-09-17: `tool_use_id` on the post hooks, `duration` in milliseconds, `failure_type`
values), not yet live-verified. Unit tests cover content exclusion, validation, the
dropped no-name-no-id payload and install/uninstall merging.

The platform's terma-cli log adapter (`termacli_logs.go`) does not parse
`terma.tool.call` yet. It needs a `tool_call` kind with `ToolCallId = tool_call_id`,
`ToolName = tool_name`, a status from `status` and `is_interrupt` (an interrupt is
`cancelled`, not a provider error) and `duration_ms` as the typed duration — and,
unlike `terma.files.touched`, no derived id when the native one is present.

## Ordering and delivery

The state file lives under the private Terma config directory in
`cursor-observations/`, keyed by conversation and repository. A lock serializes
concurrent hooks; waiting is limited to one second (or the enclosing deadline).
A timeout records `terma.session.capture` with `lock_timeout` when spooling is
possible. Overlapping hooks are ordered by local lock acquisition, not by an
assumed provider chronology. Keep generation IDs when joining; do not stitch
missing or mismatched IDs together solely by time.

A write-ahead checkpoint stores one pending observation before append. The next
hook recovers it after an append failure or crash. A crash after append may replay
the same ID: consumers must deduplicate `observation_id`. Later observations are
not acknowledged ahead of pending evidence. A corrupt checkpoint starts a new
stream and marks `capture_gap=invalid_checkpoint`. State older than 14 days is
eligible for cleanup when a new conversation is seen; a resumed pruned session
starts a new stream. A pending final observation needs another hook to recover;
there is no background checkpoint reader.

Adjacent identical snapshots are suppressed for up to ten minutes. Hook, turn,
account, model, loop, value and missingness changes all retain new positions.
Multiple changed observations in one turn are therefore retained. Separate
streams have no shared sequence. This is not a record of every model request.

Response, Stop and session end start a detached flush. Hooks themselves perform no
network requests and emit no output or agent-control response. Stop uses
`loop_limit: null` because Cursor otherwise skips it after five continuation loops.
`TERMA_HOOKS=0` disables capture. There is no status-line wrapper for Cursor.

## Real cost and funding

The backend still needs a Cursor integration; these observations do not yet feed
`terma usage` counters. Cursor's Enterprise OTLP export is configured server-side,
not by setting a local CLI exporter. Request logs offer conversation and usage-event
joins; corrections can invalidate earlier billed requests. The Admin API exposes
usage records and charged cents. A team API key is not required for local hooks.

Use provider billing records at their actual grain. Conversation-level joins can
associate costs with a session; without a generation/request join they cannot
prove the cost of each user turn. Timestamp allocation is an estimate. Neither
account email, OAuth/API authentication, context occupancy, nor a zero/missing
charge proves that subscription allowance rather than credits paid. Keep model
reference value, credits consumed and invoiced charges separate. The standalone [billing importer](../pocs/cursor-billing/README.md) reads these
records for a future platform worker; it is independent of the Terma CLI and hooks.

## Verification and sources

- Inspected official CLI package `2026.09.10-fd3934a`, darwin/arm64, from the
  [Cursor installer](https://cursor.com/install) on 2026-09-16. Its hook dispatchers
  forward the four optional snake-case token fields on response and Stop.
  Download was isolated under `/tmp`; no global CLI installation was changed.
- [Hook reference](https://cursor.com/docs/hooks): shared fields, hook configuration,
  generation IDs and compaction fields. The public reference currently omits the
  optional token fields, so they remain a version-sensitive surface.
- [Cursor staff clarification](https://forum.cursor.com/t/how-to-obtain-token-usage-per-request/168317/11):
  parent-turn hook usage excludes subagent usage.
- [Hook reference, subagent hooks](https://cursor.com/docs/agent/hooks): `subagentStop`
  fields and `loop_limit`; `subagentStart`'s "no output blocks the subagent" rule. The
  `subagentStop` wiring is documented-contract only, not yet live verified.
- [Enterprise OTLP wire contract](https://cursor.com/docs/enterprise/opentelemetry-export/wire)
  and [Admin API](https://cursor.com/docs/account/teams/admin-api): separate billing
  integration surfaces, not proof of a local funding signal.

Unit tests exercise two generations with identical usage, changes within a turn,
missing/zero/invalid values, private-content exclusion, concurrent capture,
crash replay, failed append recovery, corrupt state and install/uninstall merging.
The synthetic `live/TestCursorHookDelivery` passed on 2026-09-16 through the
installed shims, rebuilt Terma binary and loopback OTLP receiver, including the
kill switch and explicit zero preservation. The opt-in
`live/TestCursorCLITurnObservations` covers two authenticated headless CLI turns,
raw-hook-to-OTLP preservation and commit attribution. It skips without a CLI key;
an offline pass is not an authenticated provider compatibility result.

For IDE validation, install these hooks in a scratch bound repository, trust it,
then submit two prompts that edit a file. Check that response/Stop observations
retain the supplied conversation and generation IDs, optional tokens, and version,
then commit and verify attribution. Repeat after a model switch and after a
cancelled generation. Authenticated headless tests on 2026-09-16 with CLI `2026.09.08-6caf4ff` and
`2026.09.10-fd3934a` completed edits but emitted only lifecycle hooks; the strict
turn-hook test failed due to missing Stop hooks. IDE behavior remains unverified.
The separate `TestCursorBillingSession` passed on `2026.09.10-fd3934a`: two edits,
one resumed conversation and hook-delivered account/session identity. This
establishes a conversation billing join, not turn-hook or file-hook parity.


## Admin API probe (2026-09-16)

A personal key authenticated to `/v1/me` but returned `Invalid Team API Key` for
`/teams/members`. A separate team Admin API key then returned HTTP 200 for
`/teams/members`, `/teams/spend`, and `/teams/filtered-usage-events` on the team's
user-reported Teams subscription. This establishes access for this account;
Cursor's API overview and team dashboard documentation disagree on plan access.

The sampled spend row contains populated `billingTier`, `apiPercentUsed`,
`autoPercentUsed`, `totalPercentUsed`, `includedSpendCents`, `spendCents` and
`hardLimitOverrideDollars`; `monthlyLimitDollars` is present but null. The response
also has `subscriptionCycleStart`. Validate tier codes, percentage denominators,
periods and limit semantics against the dashboard before treating these as seat
or remaining-allowance facts. No separate credit-balance/reset field was observed.

The sampled usage event has a conversation ID, model, four token buckets,
`tokenUsage.totalCents`, `kind`, `isChargeable`, `chargedCents` and `cursorTokenFee`.
Its billing category is `Included in Business`. This is a path to session-level
reference value and billing attribution. Pagination and a controlled conversation
join are now verified; dashboard/invoice reconciliation remains outstanding. It does not establish full usage coverage or per-turn accuracy.

Admin API access does not change the authenticated IDE/CLI verification status
above. The reusable importer described below has since been implemented.


## Reusable platform importer (2026-09-16)

`pocs/cursor-billing` is a standalone Go module with injected credentials and
explicit tenant/connection scope. It reads members, paginated spending and
paginated usage, retaining decimal precision, provider timestamps, raw records
and unknown values. The platform owns key entry/storage, scheduling and atomic
replacement of fixed usage windows; the local runner is only a validation adapter.

The live import returned 3 members, 3 spend rows across 2 pages and 33 usage events
across 17 pages. Two events joined to the controlled two-turn conversation with
matching account identity, complete prices and token-bucket values matching the
CLI responses. Both events were `Included in Business`, yet had nonzero
`chargedCents` and `isChargeable=true`: neither field alone proves extra cash spend.
There is no authoritative generation join here; matching token tuples in this
controlled case must not become a production per-turn matching heuristic.

See the [integration/persistence contract](../pocs/cursor-billing/README.md).
Dashboard comparison of tier/seat/percentage/limit semantics is still pending;
no allowance denominator, reset date, seat price or credit balance is invented.


## Turn-hook diagnosis and interactive verification (2026-09-16)

`TestCursorInteractiveTurnObservations` now passes with CLI
`2026.09.10-fd3934a`: two turns in one terminal session, distinct generation IDs,
prompt/response/Stop hooks, all four optional token fields, ordered OTLP delivery
and file/commit attribution. The initial test exposed a delivery race; it now
waits for both Stop generations to arrive before asserting.

The inspected interactive UI explicitly invokes the turn hooks. The headless
`--print` runner lacks those calls and receives no hook executor/config. Its
session lifecycle callbacks explain why the earlier tests observed only lifecycle
hooks. Changing output format does not change that runner. Keep the strict
headless test failing until Cursor supports the missing hooks; no hook events
are synthesized from native output. IDE validation remains separate.

Interactive hook input tokens are cache-inclusive in this version; headless JSON
subtracts cache-read/write tokens. Preserve the original fields and source before
normalizing. The billing API still joins at conversation scope rather than turn
scope.
