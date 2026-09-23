# Funding evidence on the wire

The CLI collects evidence; the backend decides funding. Native request telemetry
remains the source of token usage and reference cost. The events below enrich it.
They do not create another usage counter or report an invoice amount.

## Events

All events use the existing spool and OTLP delivery, with session ID, event time,
project binding, tool and Terma version. New evidence carries `schema_version=1`.

`account_id` is the harness account that owns a subscription snapshot, stamped
directly on the evidence only when the effective credential is the subscription
login. It — and, for Claude, the cached `account_email` — is withheld, never
guessed, whenever another credential outranks it:
for Claude when an env API key/auth token, a cloud provider (Bedrock/Vertex/Foundry)
or a configured `apiKeyHelper` is in effect; for Codex when `auth.json` is not the
`chatgpt` route, or when a record carries no present ChatGPT rate-limit evidence
(a `rate_limits:null` snapshot, or a session run through `--oss`/a custom
`model_provider`, whose usage is not this account's).

| Event | Trigger | Evidence |
|---|---|---|
| `terma.session.account` | Claude SessionStart, Stop, StopFailure | Stored account/organization IDs, billing type, organization type, seat tier, extra-usage policy; visible credential configuration hints |
| `terma.session.quota` | Claude status line | Prompt ID, model, fast-mode setting, cumulative session cost, window percentages and Unix-second reset times |
| `terma.session.quota` | Codex notify, Stop, PostToolUse, SessionEnd | Plan, primary/secondary windows, credit flags/balance, limit category and spend-control flag |
| `terma.session.capture` | Codex capture cannot catch up | Backlog, incomplete record, or read/discovery failure; separate from quota state |
| `terma.session.limit` | Claude StopFailure | Allowlisted `error_type`; no error details or assistant text |

### Claude account snapshots

`evidence_source=claude_account`. `evidence_status` describes the account read:
`present`, `missing`, `unreadable`, `malformed`, `oversized` or `unsupported`.
The file is `~/.claude.json`, or `$CLAUDE_CONFIG_DIR/.claude.json` when configured.
Only `oauthAccount` fields are selected:

- `account_id`, `organization_id`, `billing_type`, `organization_type`, `seat_tier`
- `extra_usage_enabled`, `extra_usage_disabled_reason`
- `user_rate_limit_tier`, `organization_rate_limit_tier`, `organization_role`
- `account_email`: the signed-in email, the per-user identity behind the account.
  Emitted raw and renamed to `user.email` downstream; a bounded shape check gates its
  shape. This is deliberate per-user identity — the one piece of PII the funding record
  carries — and, like `account_id`, it is withheld when another credential outranks the
  cached OAuth login (an env API key/auth token, a cloud provider, a visible external
  OAuth token, or a configured `apiKeyHelper`), so a session run under a different
  credential is never stamped with the cached login's email.

Credential hints describe the hook's view, not the request's effective route:

- `api_key_present`, `auth_token_present`
- `bedrock_enabled`, `vertex_enabled`, `foundry_enabled`
- `api_key_helper_state`: `configured`, `not_found` or `unknown`, based on user and
  repository settings files; command-line and managed-preference overrides are
  not resolved. `hint_scope=hook_env_and_settings_files` names this limit.
- `oauth_token_visibility`: normally `unavailable`, because Claude strips the
  variable from hooks. If it is visible, `oauth_token_present=true` is recorded.

Missing policy fields stay absent. A missing OAuth profile still yields visible
credential hints, which is useful on API routes. No credential values, helper
commands or conversation content are emitted. The signed-in `account_email` is the
sole PII exception — captured deliberately as per-user identity when the OAuth login is
the effective credential, and withheld under the same override check as `account_id`
otherwise (see above). The helper is never executed. Account files are limited to 2 MiB
and must be regular files.

Snapshots are reread each turn and emitted on change or after a ten-minute
heartbeat. State contains only a hash and timestamp; Unix writers use a
nonblocking advisory lock. A failed append does not advance deduplication state.

### Quota snapshots

Claude uses `evidence_source=claude_statusline` and these existing field names:
`prompt_id`, `session_cost_usd`, `fast_mode`, `model`, `claude.version`, and
`<window>_used_pct` / `<window>_resets_at` for `five_hour`, `seven_day` and, when
available, `spend_limit`. Changes to the prompt, resets or cost now produce a
snapshot even when percentages are unchanged. Identical redraws are suppressed.
Each emitted Claude snapshot carries `observation_id`, `source_stream` and a
session-local `observation_sequence`. The sequence orders capture under a lock;
`prompt_id` groups observations from the same prompt. `time_basis=observed` makes
clear there is no provider timestamp in this payload. A disappearance of windows
after earlier evidence emits `evidence_status=unavailable`, invalidating stale
quota. Identical redraws remain suppressed; this is a sequence of received changes,
not a complete record of every model call. A contended capture lock skips that
render's capture; the next redraw can retry its state. Non-Unix locking remains
best-effort, as for the existing spool.

Percentages can exceed 100: the live suite observed Claude report 107%.

Codex uses `evidence_source=codex_rollout`. `source_time` is the timestamp in the
rollout; the event time is when Terma observed it. Fields are:

- `plan_type`, `limit_id`, `rate_limit_reached_type`, `spend_control_reached`
- `primary_used_pct`, `primary_window_minutes`, `primary_resets_at`
- `secondary_used_pct`, `secondary_window_minutes`, `secondary_resets_at`
- `has_credits`, `credits_unlimited`, `credits_balance`

`credits_balance` remains the provider's decimal string, retaining its precision
and units; it is not relabelled as USD. Null windows and invalid/missing scalar
values stay absent. A latest explicit null quota record yields
`evidence_status=unavailable`, replacing an older positive snapshot.

The reader confines paths to the current Codex home's `sessions/` or
`archived_sessions/`, verifies the filename and session metadata ID, and reads
at most 64 KiB of metadata plus a 1 MiB incremental chunk per invocation (up to
256 evidence records). Discovery without a transcript hint is capped at 4,096
entries. Capture has a one-second context budget inside Stop's three-second limit.

A cursor under `funding-cursors/` records file identity, acknowledged byte offset,
a checkpoint hash, and active turn ID. It starts at the beginning, not the tail.
Every `token_count.rate_limits` observation is emitted, even when values and source
timestamps repeat. Each has `source_stream`, `source_offset`, and a stable
`observation_id` for downstream retry deduplication. Cursor advancement follows
successful spooling; a crash before checkpointing can replay the same IDs. The
backend must deduplicate these IDs; exactly-once delivery is not claimed.

Turn association comes from rollout `task_started` / `turn_started` and
`turn_context` records; completion/abort clears it. `turn_id` is omitted when
unknown, including after malformed/skipped records. The parser follows the
[versioned Codex protocol](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/protocol/src/protocol.rs).
The hook's current turn is never applied to historical backlog.

Partial trailing lines wait for another capture. A bounded chunk/event limit leaves
backlog for the next PostToolUse, Stop or SessionEnd; no background rollout watcher
is installed. If no further hook fires, that backlog remains local. Capture progress
uses `terma.session.capture` so backlog cannot masquerade as missing provider quota.
A fully scanned rollout without any quota observation reports `not_ready`.
Oversized/malformed records and detected replacement/truncation emit quota events
with `evidence_status=gap` and `gap_reason`; turn association is cleared. Records
larger than 1 MiB are skipped incrementally to the next newline. Unix file identity
survives an archive rename; other platforms use metadata/checkpoint hashes and
cannot distinguish a byte-identical replacement. These checks do not detect every
possible historical in-place edit. No transcript text is stored in cursor state.
(The rollout has one other reader, `harness.ReadCodexReplies`, which does read text — the
assistant's replies, for a developer whose prompts already travel — with a cursor of its
own under `reply-cursors/` that holds none either. Funding capture itself reads none.)

Collection records observations, not billed amounts or request boundaries. Multiple
snapshots can share observation time; consumers must use source ordering to break
ties. A post-response quota observation is not proof of the preceding call's
starting state. Shared credit balances remain unsuitable for assigning an entire
balance decrease to one call without further evidence.

### Failure categories

The parser follows the [Claude StopFailure contract](https://code.claude.com/docs/en/hooks#stopfailure).
It keeps documented categories and maps new categories to `unknown`. It discards
`error_details` and `last_assistant_message`. A controlled live HTTP 400 test on
Claude 2.1.272 was reported by Claude as `unknown`; it must not be relabelled.

The Codex [Stop input schema](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/hooks/schema/generated/stop.command.input.schema.json)
and versioned rollout fixtures are the reader's source contract. Rollout format
compatibility is checked by the live suite, not assumed across versions.

## Delivery and verification

Account/plan capture finishes before each Stop hook or Codex end-of-turn notify starts a detached flush.
There are two routes to the same capture, sharing one locked per-session cursor so nothing
is counted twice. A machine-wide `terma connect codex` installs the user-level notify, which
does not depend on repository hooks. A repository routed by `terma install` runs Codex in a
per-project Codex home that keeps the developer's own notifier, so there the repository's
`.codex` hooks (Stop, PostToolUse, SessionEnd) do the capturing. If Codex already has a notifier, Terma
chains it after capture and restores it on disconnect; restoration replays the same
program and arguments, normalized to a single-line array, so a notifier originally
written as a multiline array or carrying inline comments comes back reformatted.
Codex Stop is synchronous with a three-second harness timeout so local capture
can finish before `codex exec` exits. Hooks perform no network calls themselves.
Claude can render after Stop, so a newly queued status-line snapshot also starts
a detached flush. Sender failure backoff still applies. `TERMA_HOOKS=0` prevents
capture and delivery while preserving the user's status-line renderer (without the
`t` prefix). With capture enabled, nonempty custom output gets the prefix; without
a previous renderer, capture is silent. Empty custom output stays empty. See
[status-line compatibility](STATUSLINE-COMPATIBILITY.md) for test coverage and limits.

Each flush has a Terma-owned **30-second deadline** covering lock acquisition and
all batches/projects. Individual HTTP requests still have a 15-second limit.
On Unix, a separate delivery lock serializes flushes; the queue's append lock is
held only for local reads/rewrites, never for network requests. Lock waits observe
cancellation; append and peek waits are capped at 250 ms. Platforms without flock
retain the existing best-effort locking fallback.

Successful batches are acknowledged with one atomic rewrite that preserves both
concurrent appends and held events. Cancellation before acknowledgement leaves the
original batch for retry: delivery remains at least once, so a remotely accepted
batch can be replayed. Sender failures retain the batch and apply backoff. There
is no background retry timer; a later hook or `terma spool flush` triggers retry.
The separate renderer deadline is described in the compatibility guide above.

The live suite checks hook-delivered snapshots, timestamp/prompt joins and two
Claude turns while the session stays open. Request contracts allow empty Codex
response records before a reply, while requiring positive completed-turn usage. A real Claude process against a
synthetic loopback API error verifies StopFailure delivery without exhausting an
account. An isolated interactive scenario uses a controlled streaming response
to verify the original renderer and Terma mark, with a preapproved dummy API key
in its temporary config. Offline tests cover account changes, duplicate hooks, unknown fields,
bounded reads, incomplete writes, foreign paths and secret exclusion.

Run `cd live && make test` for offline contracts, or `make live` with the local
credential configuration for real harnesses. Missing credentials remain explicit
skips. Backend mappings and billing reconciliation are subsequent work.


Authentic API routes passed on 2026-09-15 for Claude 2.1.270–2.1.272 and Codex
0.153.3–0.154.0. Claude delivers headless per-request usage and credential-presence
account evidence; Codex delivers completed-response usage with `auth_mode=ApiKey`
and its hook's quota-availability status. Missing API quota is not zero quota.
The corresponding field-name baselines are `live/golden/claude/*-apikey.json` and
`live/golden/codex/response_completed-apikey.json`. This verifies collection;
provider billing reconciliation and mixed-auth live coverage remain separate.

## Cursor IDE and CLI

Cursor now records ordered, generation-keyed hook observations, with optional
parent-turn token snapshots and account email. These do not expose plan, credit
balance or funding transitions. They use `terma.session.observation` so they cannot
be confused with provider quota or native additive usage counters. See
[the Cursor schema, delivery contract and billing join limits](CURSOR-INSTRUMENTATION.md).
