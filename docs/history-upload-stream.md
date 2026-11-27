# `terma history upload --json-stream` Contract

This document specifies the machine-readable progress stream produced by `terma history upload --json-stream`, consumed by Terma Desktop.

## Invocation

```bash
terma history upload --json-stream [--since <window>] [--active-window <duration>] [--dry-run] [--retry-failed]
```

- `--json-stream` cannot be combined with `--output` / `-o` (including an empty value such as `--output=`).
- `--active-window` defaults to `1h` and accepts whole seconds or `0` (disabled). Sub-second values such as `500ms` or `1.5s` are rejected so `active_window_seconds` is always exact. Note that Go's `time.ParseDuration` truncates sub-nanosecond fractions to integer nanoseconds before the whole-second check (for example, `0.1ns` truncates to `0` and `1.0000000001s` truncates to `1s`).
- Stdout carries **only** newline-delimited JSON objects (NDJSON), one object per line terminated by `\n`.
- Stderr carries human-readable diagnostics (such as error summaries) and must not be parsed as JSON.
- **Schema versioning:** `start.stream_version` is currently `1`. New event types, new optional fields on existing events, or new values in open string sets (`error.code`, `team_blocked.reason_code`, `session_skipped.reason`, skip-reason map keys) may be added within version `1`; readers must ignore unknown event types, unknown fields, and unrecognized string codes. If a consumer sees `stream_version > 1`, a breaking schema revision is in effect: do not assume the version `1` contract holds — surface a version-mismatch error prompting the user to update the consumer or CLI rather than misparsing events.

## Failures Before the Stream Starts

Some invocation failures happen before `history upload` runs (or inside Cobra before `--json-stream` is recognized) and therefore cannot write JSON to stdout:

- Malformed boolean syntax on `--json-stream` itself (e.g. `--json-stream=maybe`).
- `--json-stream` consumed as the value of a preceding flag (e.g. `--since --json-stream`).
- `--json-stream` placed on the parent command instead of `upload` (e.g. `terma history --json-stream upload`).
- An invalid root/persistent flag before `upload`, an older `terma` binary that does not recognize `--json-stream`, or `--help` (which prints help text and exits `0`).

**Consumer rule for failed starts:** If the process exits non-zero and stdout is empty (no JSON lines), the command failed before the stream started — treat the run as failed and show stderr to the user.

Any error that `history upload` observes after its flags are parsed — including unknown `upload` flags, extra positional arguments, `--output=` / `-o`, invalid `--since` or `--active-window`, conflicting `GIT_*` environment variables, or a config/state directory resolution failure (such as `HOME` unset) — emits a single `error` JSON line on stdout (`code: "failed"`, exit `1`) and no `start` or `summary`.

## Unit of Progress ("N of M") and Byte Fields

- **The progress unit is one session-team upload.** A single transcript file whose segments route to two different teams produces **two** session-team uploads: `plan.sessions` is `2`, the upload emits `index: 1, total: 2` and `index: 2, total: 2`, and `summary.sent_sessions` is `2`.
- **Reconciliation invariant (completed, non-interrupted run):** Every session-team upload counted in `plan.sessions` reaches exactly one terminal outcome:
  - one `session_done` line,
  - one `session_failed` line,
  - one `session_skipped` line, or
  - inclusion in a post-`plan` (`phase: "upload"`) `team_blocked` line's `skipped` count (including a session that triggered a route block after its `session_start`).
- **`source_bytes` vs `sent_bytes` (not comparable):**
  - `source_bytes` is an **estimate** derived from transcript file sizes on disk before routing, content filtering, and OTLP protobuf encoding.
  - `plan.source_bytes` deduplicates by transcript file so a transcript split across two teams is counted **once** in the top-level `plan.source_bytes` (while each team entry in `plan.teams` reports the `source_bytes` of its own sessions).
  - `session_done.sent_bytes` is the exact encoded OTLP request payload sent over HTTP.
  - **Sums rule:** On any run that reaches a complete `summary` line without write drop or truncation (including an interrupted run whose reader kept draining stdout within the cancel grace period), the count of `session_done` lines equals `summary.sent_sessions`, the sum of `session_done.records` equals `summary.sent_records`, and the sum of `session_done.sent_bytes` equals `summary.sent_bytes`.
  - Do not compute byte progress percentages as `sent_bytes / plan.source_bytes`; drive progress bars from session counts (`completed / plan.sessions`).
  - `estimated_bytes` appears only in `--dry-run` plans and reflects estimated encoded OTLP request bytes.

## Ordering Rules

A real upload emits events in this order:

1. `start` — emitted once the per-profile upload lock is acquired and resume state is loaded (a `--dry-run` emits `start` after loading resume state, **without** taking the upload lock).
2. `discover` *(optional on early cancel/failure)* — emitted once local transcript discovery finishes. If the run is cancelled or fails fatally during discovery, `summary` (and `error` if fatal) follows `start` directly with neither `discover` nor `plan`.
3. Optional discovery-phase `team_blocked` lines (`phase: "discovery"`) — emitted once per blocked team (sorted by `enrollment_id`) when local routing holds sessions (including older resumable sessions outside `--since`) for a team already blocked in local routing state. These sessions are not in `plan` and are counted in `summary.skipped_segments`, not `summary.team_issues`.
4. Planning phase (omitted in `--dry-run`, optional on early cancel/failure): for each remaining team in sorted `enrollment_id` order, `holdings` and/or planning-phase `team_blocked` lines (`phase: "plan"`).
   - A team whose only work is resumed/retried sessions (no new candidate sessions) emits neither `holdings` nor a holdings-check `team_blocked` (it may still emit a `plan`-phase `team_blocked` for a missing server key, an already-blocked organization, or a late organization refusal triggered by another team in the same organization).
   - If team B's holdings check returns `ORG_NOT_ACCESSIBLE` after team A in the same organization already passed holdings, team B's `team_blocked` (`phase: "plan"`) is emitted first (when team B fails), and team A's `team_blocked` (`phase: "plan"`) is emitted second (when the organization block is retroactively applied before `plan`). None of those blocked teams' sessions appear in `plan`.
   - If the run is cancelled or fails fatally during planning (for example `signed_out` during a holdings check), `summary` (and `error` if fatal) is emitted with no `plan`.
5. `plan` *(optional on early cancel/failure)* — emitted once after all teams are planned and before the first session is uploaded. `plan.sessions` only counts work that will actually be attempted. A team with `sessions == 0` and an empty `skipped_sessions` map is omitted from `plan.teams`.
6. Upload phase (omitted in `--dry-run`): teams are uploaded in sorted `enrollment_id` order; within each team, resumed/retried sessions (`in_progress` and `--retry-failed` failures) are sent before new sessions. For each planned session-team upload:
   - `session_start` followed by `session_done`, `session_failed`, `session_skipped` (including `mixed`, `empty_after_filter`, or mid-read `active`/`missing_file`/`unreadable`), or a `team_blocked` (`phase: "upload"`) event that stops the team; **or**
   - `session_skipped` **without** a preceding `session_start` when the pre-read stat/active check skips the session (`active`, `missing_file`, or `unreadable`); **or**
   - `team_blocked` (`phase: "upload"`) **without** a preceding `session_start` when a pre-session route check or an earlier team's organization block stops the team (or after 5 consecutive server rejections on preceding sessions).
7. Terminal line(s): `summary`, or `summary` followed by `error`.

A `--dry-run` acquires no upload lock, makes no network calls, and emits: `start`, `discover`, optional discovery-phase `team_blocked`, `plan` (`holdings_checked: false`), `summary`.

## Terminal-Line and Success Rules

**Success requires both a complete newline-terminated `summary` line (with no trailing `error`) and exit code `0`.** The process never exits `0` unless the final line was written completely.

| Outcome | Lines emitted | Exit code |
| --- | --- | --- |
| Failure before `history upload` sees `--json-stream` (bad root flag, `--json-stream=maybe`, `--since --json-stream`, `history --json-stream upload`, older CLI) | Empty stdout (non-JSON error on stderr) | Non-zero (usually `1`) |
| `--help` | Help text on stdout (not JSON) | `0` |
| Fatal error before work begins (`upload_running`, `not_set_up`, `signed_out` at start including retired sign-in with nothing to send, bad flags/args, `--output=`, `HOME` unset, `GIT_*` set) | `error` only (no `start`, no `summary`) | `4`, `5`, `6`, or `1` |
| Fatal error after `start` (sign-in lost or retired mid-run, fatal state write error) | `start` … `summary`, then `error` | `6` (`signed_out`) or `1` (`failed`) |
| Completed run with no new session failures or `plan`/`upload` team issues | `start` … `summary` | `0` |
| Completed run with one or more new session failures or `plan`/`upload` team issues | `start` … `summary` | `1` |
| Signal (`SIGINT`/`SIGTERM`) after the last session has already finished uploading | `start` … `summary` (`interrupted` absent/false) | `0` or `1` (normal completion) |
| Interrupted run (`SIGINT` or `SIGTERM` after `start` while work remains) | `start` … `summary` (`summary.interrupted: true`) | `130` (`SIGINT`) or `143` (`SIGTERM`) |
| Stdout write failed or timed out after cancel (250 ms grace expired) | Stream may stop before `summary` | Non-zero (`130`/`143` on signal, `1` otherwise) |
| Second `SIGINT`/`SIGTERM`, `SIGKILL`, crash, OOM, or reader closed stdout (`SIGPIPE`) | Stream ends with neither `summary` nor `error` (last line may be unterminated) | Non-zero / signal |

**Reader rules for incomplete or unterminated streams:**
- Every valid JSON line ends with `\n`. If EOF is reached with trailing bytes not terminated by `\n`, discard that unterminated last line.
- If the last complete line is neither `summary` nor `error` (or stdout is empty with a non-zero exit), the process did not finish cleanly; treat the run as failed and allow retry (resume).

## Error Codes and Exit Codes

`error.code` is an **open set**; new codes may be added over time and consumers must handle unknown codes as generic fatal failures (showing `error.message`). Values emitted today:

| `error.code` | Exit code | Meaning |
| --- | --- | --- |
| `upload_running` | `4` | Another `terma history upload` holds this profile's upload lock. |
| `not_set_up` | `5` | Terma is not set up on this machine, or routing state belongs to another account. |
| `signed_out` | `6` | Not signed in at start, sign-in already retired at start, or sign-in lost/expired/retired during the run. |
| `failed` | `1` | Invalid flags/arguments, `--output` combined with `--json-stream`, directory-resolution failure (`HOME` unset), conflicting `GIT_*` environment variables, or another fatal error. |

Non-`error` exit codes:
- `0`: completed cleanly with a complete `summary` line and no new failures or `plan`/`upload` team issues.
- `1`: completed with new session failures or `plan`/`upload` team issues (last line `summary`), or stdout write error / non-signal cancellation without a complete final line.
- `130`: interrupted by `SIGINT` (last line `summary` with `interrupted: true`, unless stdout stalled past the 250 ms grace period).
- `143`: interrupted by `SIGTERM` (last line `summary` with `interrupted: true`, unless stdout stalled past the 250 ms grace period).

## Event Reference

### `start`

Emitted once resume state is loaded (and, outside `--dry-run`, once the per-profile upload lock is held), before discovery starts.

Fields:
- `type` (`string`): `"start"`
- `stream_version` (`int`): schema version, currently `1`
- `dry_run` (`bool`): whether `--dry-run` was set
- `since` (`string`): exact UTC cutoff timestamp used for discovery, formatted as RFC 3339 / RFC 3339 Nano (`2006-01-02T15:04:05.999999999Z`)
- `active_window_seconds` (`int64`): active-session window in whole seconds (`0` when disabled)
- `agents` (`array`): supported agents — `{name, display_name, history}` (`string`, `string`, `bool`) where `history` indicates whether terma can read that agent's past sessions

Example:
```json
{"type":"start","stream_version":1,"dry_run":false,"since":"2026-08-29T12:00:00Z","active_window_seconds":3600,"agents":[{"name":"claude","display_name":"Claude Code","history":true},{"name":"codex","display_name":"Codex","history":true},{"name":"omp","display_name":"Oh My Pi","history":true},{"name":"opencode","display_name":"OpenCode","history":false}]}
```

### `discover`

Emitted once local transcript discovery finishes.

Fields:
- `type` (`string`): `"discover"`
- `files` (`int`): transcript files found
- `skipped_sessions` (`object`, `map[string]int`, always present, `{}` when empty): sessions skipped during discovery by skip reason (open set; values emitted today: `active`, `outside_since`, `unreadable`, `missing_file`, and `record_too_large` on `--dry-run` only)

Example:
```json
{"type":"discover","files":12,"skipped_sessions":{"active":1,"outside_since":3}}
```

### `holdings`

Emitted before asking a team's API which new candidate sessions it already holds. Not emitted in `--dry-run` or for teams that have only resumed/retried sessions.

Fields:
- `type` (`string`): `"holdings"`
- `enrollment_id` (`string`)
- `team_id` (`string`)
- `team_label` (`string`)
- `sessions` (`int`): candidate sessions checked for this team

Example:
```json
{"type":"holdings","enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","sessions":4}
```

### `plan`

Emitted once after all teams are planned and before the first upload begins.

Fields:
- `type` (`string`): `"plan"`
- `holdings_checked` (`bool`): `true` on real uploads, `false` on `--dry-run`
- `sessions` (`int`): total session-team uploads to attempt
- `resume_sessions` (`int`): subset of `sessions` resuming from `in_progress` state or retried via `--retry-failed`
- `records` (`int`): record count (pre-filter on real upload, post-filter on `--dry-run`)
- `source_bytes` (`int64`): deduplicated on-disk transcript file bytes (estimate only; not comparable to `sent_bytes`)
- `estimated_bytes` (`int64`, optional): estimated encoded OTLP bytes (`--dry-run` only; omitted when `0` or on real uploads)
- `skipped_sessions` (`object`, `map[string]int`, always present, `{}` when empty): sessions skipped during planning (open set; values emitted today: `already_done`, `failed_previously`, `already_held`)
- `teams` (`array`, always present, `[]` when empty): per-team breakdown for teams with `sessions > 0` or non-empty `skipped_sessions`:
  - `enrollment_id` (`string`), `team_id` (`string`), `team_label` (`string`)
  - `sessions` (`int`), `resume_sessions` (`int`), `records` (`int`), `source_bytes` (`int64`)
  - `estimated_bytes` (`int64`, optional; `--dry-run` only)
  - `skipped_sessions` (`object`, `map[string]int`, always present)

Example:
```json
{"type":"plan","holdings_checked":true,"sessions":3,"resume_sessions":1,"records":42,"source_bytes":18432,"skipped_sessions":{"already_done":2,"already_held":1},"teams":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","sessions":3,"resume_sessions":1,"records":42,"source_bytes":18432,"skipped_sessions":{"already_done":2,"already_held":1}}]}
```

### `session_start`

Emitted immediately before reading and streaming a planned session-team upload.

Fields:
- `type` (`string`): `"session_start"`
- `index` (`int`): 1-based position in `plan.sessions`
- `total` (`int`): equals `plan.sessions`
- `enrollment_id` (`string`), `team_id` (`string`), `team_label` (`string`)
- `source_system` (`string`), `session_id` (`string`)
- `source_bytes` (`int64`): size of this session's transcript files on disk (`0` if unreadable at plan time)

Example:
```json
{"type":"session_start","index":1,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"11111111-1111-1111-1111-111111111111","source_bytes":6144}
```

### `session_done`

Emitted when a planned session-team upload finishes successfully. Always follows `session_start`.

Fields:
- All fields of `session_start`
- `records` (`int`): OTLP records sent
- `sent_bytes` (`int64`): encoded OTLP request bytes sent (reconciles with `summary.sent_bytes`)

Example:
```json
{"type":"session_done","index":1,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"11111111-1111-1111-1111-111111111111","source_bytes":6144,"records":14,"sent_bytes":2910}
```

### `session_failed`

Emitted when a session permanently fails — either because the server permanently rejected it (HTTP 4xx or OTLP partial rejection) or because of a local permanent failure with no server request involved (for example, a single record exceeding the maximum OTLP request size). Always follows `session_start`. The failure is recorded on disk and skipped as `failed_previously` on later runs unless `--retry-failed` is passed.

Fields:
- All fields of `session_start`
- `reason` (`string`, optional): failure message (omitted when empty)

Example:
```json
{"type":"session_failed","index":2,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"22222222-2222-2222-2222-222222222222","source_bytes":5120,"reason":"HTTP 400: payload rejected"}
```

### `session_skipped`

Emitted when a planned session is skipped during the upload phase.
- May occur **without** a preceding `session_start` for pre-read stat/active checks (`active`, `missing_file`, `unreadable`).
- Occurs **after** `session_start` for `mixed`, `empty_after_filter`, and mid-read/post-read `active`, `missing_file`, or `unreadable`.
- `record_too_large` is **not** emitted on `session_skipped`: it appears only in `--dry-run` discovery skips (`discover.skipped_sessions` / `summary.skipped_sessions`), whereas on a real upload an oversized record emits `session_failed`.

Fields:
- All fields of `session_start`
- `reason` (`string`, optional): canonical skip reason (open set; values emitted today: `active`, `missing_file`, `unreadable`, `mixed`, `empty_after_filter`)

Example:
```json
{"type":"session_skipped","index":3,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"33333333-3333-3333-3333-333333333333","source_bytes":7168,"reason":"active"}
```

### `team_blocked`

Emitted when a team cannot be uploaded to during discovery, planning, or upload. May occur **without** a preceding `session_start`.

Fields:
- `type` (`string`): `"team_blocked"`
- `enrollment_id` (`string`), `team_id` (`string`), `team_label` (`string`)
- `phase` (`string`): when the block occurred:
  - `"discovery"` — team was already held in local routing state before planning; counted in `summary.skipped_segments`, **not** in `summary.team_issues`, and does not by itself cause exit `1`
  - `"plan"` — team stopped during planning (before `plan` is emitted); counted in `summary.team_issues` and `summary.skipped_sessions`, causes exit `1`
  - `"upload"` — team stopped during upload (after `plan` is emitted); counted in `summary.team_issues` and `summary.skipped_sessions`, causes exit `1`, and `skipped` reconciles against `plan.sessions`
- `reason_code` (`string`): canonical block reason (**open set**; values emitted today: `team_blocked:<state>` where `<state>` is a non-active routing state such as `suspended`, `expired`, `sign_in_needed`, `refused`, or a routing hold reason; `holdings_check_failed`; `org_access_lost`; `send_failed`; `deselected`; `policy_changed`; `not_collected`; `wrong_environment`)
- `reason` (`string`): human-readable reason
- `skipped` (`int`): count of sessions left for a later run (may be `0`)

Example:
```json
{"type":"team_blocked","enrollment_id":"enr-b","team_id":"team-b","team_label":"Acme Labs","phase":"plan","reason_code":"org_access_lost","reason":"organization is not accessible","skipped":2}
```

### `summary`

Carries the exact `Summary` object printed by `terma history upload -o json`.

Top-level fields:
- `type` (`string`): `"summary"`
- `summary` (`object`):

| Field | Type | Optional | Meaning |
| --- | --- | --- | --- |
| `dry_run` | `bool` | Yes (`omitempty`) | `true` on `--dry-run`, omitted on real uploads |
| `interrupted` | `bool` | Yes (`omitempty`) | `true` when stopped early by cancellation/signal, omitted otherwise |
| `discovered_files` | `int` | No | Transcript files examined during discovery |
| `sent_sessions` | `int` | No | Session-team uploads completed in this run |
| `sent_records` | `int` | No | OTLP records sent in completed sessions |
| `sent_bytes` | `int64` | No | Encoded OTLP request bytes sent in completed sessions |
| `planned` | `array` of `RoutePlan` | Yes (`omitempty`) | Pre-holdings / pre-plan-block per-route plan (same as `-o json`). Because `planned` is computed before holdings checks and `plan`-phase team blocks, `sum(summary.planned[].sessions)` can **exceed** `plan.sessions`. |
| `skipped_sessions` | `map[string]int` | Yes (`omitempty`) | Total sessions skipped across discovery, planning, and upload, by skip reason (open set) |
| `skipped_segments` | `map[string]int` | Yes (`omitempty`) | Segments skipped during discovery/routing (including `team_blocked:<state>`, `no_cwd`, `missing_cwd`, `origin_mismatch`, `unroutable`, `not_collected`, `wrong_environment`, `mixed`, `empty_after_filter`) |
| `withheld_records` | `int` | Yes (`omitempty`) | Records withheld at or after a session's Mixed cutoff |
| `failed` | `array` of `FailedSession` | Yes (`omitempty`) | Permanent session failures (both new in this run and stored from earlier runs) |
| `team_issues` | `array` of `TeamIssue` | Yes (`omitempty`) | Destination-level failures from `plan` and `upload` phases (never `discovery` blocks) |
| `unreadable` | `array` of `string` | Yes (`omitempty`) | Up to 3 transcript or directory paths discovery could not read |

`summary.planned[]` (`RoutePlan`):

| Field | Type | Optional | Meaning |
| --- | --- | --- | --- |
| `enrollment_id` | `string` | No | Team enrollment ID |
| `team_id` | `string` | No | Destination team ID |
| `team_label` | `string` | No | Human-readable team label |
| `sessions` | `int` | No | Pre-holdings session count for this team |
| `resume_sessions` | `int` | Yes (`omitempty`) | Subset resuming from `in_progress` or `--retry-failed` |
| `records` | `int` | No | Record count |
| `estimated_bytes` | `int64` | No | Estimated encoded OTLP bytes (`0` on real uploads; non-zero on `--dry-run`) |

`summary.failed[]` (`FailedSession`):

| Field | Type | Optional | Meaning |
| --- | --- | --- | --- |
| `enrollment_id` | `string` | No | Team enrollment ID |
| `team_id` | `string` | No | Destination team ID |
| `team_label` | `string` | No | Human-readable team label |
| `source_system` | `string` | No | Agent source system (e.g. `claude`, `codex`, `omp`) |
| `session_id` | `string` | No | Session identifier |
| `reason` | `string` | No | Permanent failure reason |
| `rejected_records` | `int64` | Yes (`omitempty`) | Number of records reported rejected by OTLP partial success |
| `previously` | `bool` | Yes (`omitempty`) | `true` when carried forward from a prior run's state (does not cause exit `1`) |
| `failed_at` | `string` (RFC 3339) | No | UTC timestamp when the failure was recorded |

`summary.team_issues[]` (`TeamIssue`):

| Field | Type | Optional | Meaning |
| --- | --- | --- | --- |
| `enrollment_id` | `string` | No | Team enrollment ID |
| `team_id` | `string` | No | Destination team ID |
| `team_label` | `string` | No | Human-readable team label |
| `skipped` | `int` | No | Number of sessions skipped due to this team issue |
| `reason` | `string` | No | Human-readable failure reason |

Example:
```json
{"type":"summary","summary":{"discovered_files":12,"sent_sessions":1,"sent_records":14,"sent_bytes":2910,"planned":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","sessions":3,"records":42,"estimated_bytes":0}],"skipped_sessions":{"active":2,"already_done":2,"already_held":1,"outside_since":3},"failed":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"22222222-2222-2222-2222-222222222222","reason":"HTTP 400: payload rejected","failed_at":"2026-11-27T12:00:05Z"}]}}
```

### `error`

Always the last line when present. Indicates a fatal error that prevented the run from finishing.

Fields:
- `type` (`string`): `"error"`
- `error` (`object`):
  - `code` (`string`): error code (open set; see Error Codes above)
  - `message` (`string`): human-readable error message

Example:
```json
{"type":"error","error":{"code":"signed_out","message":"not signed in: signed out during upload"}}
```

## Cancel and Resume Contract

- **Cancelling a run:** Send one `SIGINT` (preferred) or `SIGTERM` to the `terma` process and **keep draining stdout until EOF**.
  - On the first signal, `terma` cancels in-flight work, finishes writing queued lines and the final `summary` line (`interrupted: true`) within a single 250 ms grace period measured from the cancel, releases the profile upload lock, and exits `130` (`SIGINT`) or `143` (`SIGTERM`).
  - If the signal arrives after the last session has already finished uploading, the run completes normally (`interrupted` absent/false, exit `0` or `1`).
  - Sending a **second** `SIGINT` or `SIGTERM` force-terminates the process immediately with no final line.
  - If the reader stops draining stdout before cancelling, `terma` gives up on blocked stdout writes once the 250 ms grace period expires, releases the upload lock, and exits non-zero (`130`/`143`).
  - If the reader closes stdout early, `terma` exits on `SIGPIPE` (or a broken-pipe write error) on the next write and never exits `0`.
- **Resuming a run:** Run the same `terma history upload --json-stream ...` command again.
  - Before the first HTTP request of a session is sent, `terma` persists `in_progress` in per-route state; it marks the session `done` only after every chunk of the session succeeds. There is **no** per-chunk checkpoint: resuming an interrupted session resends from the start of that session (which may resend chunks that already succeeded; server ingest is idempotent).
  - Completed sessions are skipped in `plan.skipped_sessions.already_done`.
  - Unfinished (`in_progress`) sessions and (when `--retry-failed` is passed) earlier permanent failures are counted in `plan.resume_sessions` and uploaded first **within each team** (without a holdings check).
  - Wait for the previous process to exit (or handle `upload_running` / exit `4`) before starting a new run.
- **Handling `signed_out` (exit `6`) and `not_set_up` (exit `5`):** Prompt the user to complete sign-in / setup before re-running `terma history upload`.
