# `terma history upload --json-stream` Contract

This document specifies the machine-readable progress stream produced by `terma history upload --json-stream`, consumed by Terma Desktop.

## Invocation

```bash
terma history upload --json-stream [--since <window>] [--active-window <duration>] [--dry-run] [--retry-failed]
```

- `--json-stream` cannot be combined with `--output` / `-o`.
- `--active-window` defaults to `1h` and accepts whole seconds or `0` (disabled). Sub-second values such as `500ms` or `1.5s` are rejected so `active_window_seconds` is always exact.
- Stdout carries **only** newline-delimited JSON objects (NDJSON), one object per line.
- Stderr carries human-readable diagnostics (such as error summaries) and should not be parsed as JSON.
- Field names are stable. New event types or new fields on existing events may be added over time; readers must ignore unknown types and unknown fields.

## Unit of Progress ("N of M") and Byte Fields

- **The progress unit is one session-team upload.** A single transcript file whose segments route to two different teams produces **two** session-team uploads: `plan.sessions` is `2`, the upload emits `index: 1, total: 2` and `index: 2, total: 2`, and `summary.sent_sessions` is `2`.
- **Reconciliation invariant (completed, non-interrupted run):** Every session-team upload counted in `plan.sessions` reaches exactly one terminal outcome:
  - one `session_done` line,
  - one `session_failed` line,
  - one `session_skipped` line, or
  - inclusion in a post-`plan` `team_blocked` line's `skipped` count (including a session that failed with a route block after its `session_start`).
- **`source_bytes` vs `sent_bytes` (not comparable):**
  - `source_bytes` is an **estimate** derived from transcript file sizes on disk before routing, content filtering, and OTLP protobuf encoding.
  - `plan.source_bytes` deduplicates by transcript file so a transcript split across two teams is counted **once** in the top-level `plan.source_bytes` (while each team entry in `plan.teams` reports the `source_bytes` of its own sessions).
  - `session_done.sent_bytes` is the exact encoded OTLP request payload sent over HTTP. Across a run, the sum of `session_done.sent_bytes` equals `summary.sent_bytes`.
  - Do not compute byte progress percentages as `sent_bytes / plan.source_bytes`; drive progress bars from session counts (`completed / plan.sessions`).
  - `estimated_bytes` appears only in `--dry-run` plans and reflects estimated encoded OTLP request bytes.

## Ordering Rules

A normal upload emits events in this order:

1. `start` — emitted once the per-profile upload lock is acquired and resume state is loaded.
2. `discover` — emitted once local transcript discovery finishes.
3. Optional discovery-phase `team_blocked` lines — emitted once per blocked team (sorted by `enrollment_id`) when routing holds sessions for a team that is already blocked in local routing state. These sessions are not in `plan`.
4. Planning phase (omitted in `--dry-run`): for each remaining team in sorted `enrollment_id` order, `holdings` and/or planning-phase `team_blocked` lines.
   - If a team's holdings check returns `ORG_NOT_ACCESSIBLE`, the entire organization is blocked before `plan` is emitted, including teams in that organization whose holdings checks already passed. None of those blocked teams' sessions appear in `plan`.
5. `plan` — emitted once, after all teams are planned and before the first session is uploaded. `plan.sessions` only counts work that will actually be attempted.
6. Upload phase (omitted in `--dry-run`): for each planned session-team upload in plan order:
   - `session_start` followed by `session_done`, `session_failed`, `session_skipped`, or a `team_blocked` event that stops the team; **or**
   - `session_skipped` **without** a preceding `session_start` when a pre-upload recheck skips the session before streaming begins (for example, the transcript became active, unreadable, missing, mixed, or empty after content filtering); **or**
   - `team_blocked` **without** a preceding `session_start` when a pre-session route check stops the team (for example, policy changed, team deselected, or organization access lost).
7. Terminal line(s): `summary`, or `summary` followed by `error`.

A `--dry-run` skips network calls and emits: `start`, `discover`, optional discovery-phase `team_blocked`, `plan` (`holdings_checked: false`), `summary`.

## Terminal-Line Rule

**The last line of a completed stream is always exactly one of `summary` or `error`. If `error` is present it is last, and the run failed.**

| Outcome | Lines emitted | Exit code |
| --- | --- | --- |
| Fatal error before work begins (`upload_running`, `not_set_up`, `signed_out` at start, bad flags or environment) | `error` only (no `start`, no `summary`) | `4`, `5`, `6`, or `1` |
| Fatal error after `start` (sign-in lost or retired mid-run, fatal state write error) | `start` … `summary`, then `error` | `6` (`signed_out`) or `1` (`failed`) |
| Completed run with no new session failures or team blocks | `start` … `summary` | `0` |
| Completed run with one or more new session failures or team issues | `start` … `summary` | `1` |
| Interrupted run (SIGINT or SIGTERM after `start`) | `start` … `summary` (`summary.interrupted: true`) | `130` (SIGINT) or `143` (SIGTERM) |
| Process died unexpectedly (SIGKILL, crash, OOM, broken pipe) | Stream ends with neither `summary` nor `error` | Non-zero / signal |

**Reader rule for truncated streams:** If EOF is reached and the last line is neither `summary` nor `error`, the process died before finishing. The reader must treat the run as failed and may safely retry (resume).

## Error Codes and Exit Codes

In `--json-stream` mode, fatal `error` lines carry one of these codes and exit statuses:

| `error.code` | Exit code | Meaning |
| --- | --- | --- |
| `upload_running` | `4` | Another `terma history upload` holds this profile's upload lock. |
| `not_set_up` | `5` | Terma is not set up on this machine, or routing state belongs to another account. |
| `signed_out` | `6` | Not signed in at start, or sign-in was lost, expired/unrenewable, or retired during the run. |
| `failed` | `1` | Invalid flags/arguments, conflicting environment variables (e.g. `GIT_DIR`), or another fatal error. |

Other exit codes without an `error` line:
- `0`: completed cleanly (last line `summary`).
- `1`: completed with new session failures or team issues (last line `summary`).
- `130`: interrupted by `SIGINT` (last line `summary` with `interrupted: true`).
- `143`: interrupted by `SIGTERM` (last line `summary` with `interrupted: true`).

## Event Reference

### `start`

Emitted once the upload lock is held and resume state is loaded, before discovery starts.

Fields:
- `type`: `"start"`
- `dry_run` (`bool`): whether `--dry-run` was set
- `since` (`string`): RFC 3339 UTC cutoff timestamp
- `active_window_seconds` (`int64`): active-session window in whole seconds (`0` when disabled)
- `agents` (`array`): supported agents — `{name, display_name, history}` where `history` indicates whether terma can read that agent's past sessions

Example:
```json
{"type":"start","dry_run":false,"since":"2026-08-29T12:00:00Z","active_window_seconds":3600,"agents":[{"name":"claude","display_name":"Claude Code","history":true},{"name":"codex","display_name":"Codex","history":true},{"name":"opencode","display_name":"OpenCode","history":false}]}
```

### `discover`

Emitted once local transcript discovery finishes.

Fields:
- `type`: `"discover"`
- `files` (`int`): transcript files found
- `skipped_sessions` (`object`): map of canonical skip reason to count of sessions skipped during discovery

Example:
```json
{"type":"discover","files":12,"skipped_sessions":{"active":1,"outside_since":3}}
```

### `holdings`

Emitted before asking a team's API which candidate sessions it already holds.

Fields:
- `type`: `"holdings"`
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
- `type`: `"plan"`
- `holdings_checked` (`bool`): `true` on real uploads, `false` on `--dry-run`
- `sessions` (`int`): total session-team uploads to attempt
- `resume_sessions` (`int`): subset of `sessions` resuming from prior incomplete state
- `records` (`int`): record count (pre-filter on real upload, post-filter on `--dry-run`)
- `source_bytes` (`int64`): deduplicated on-disk transcript file bytes (estimate only; not comparable to `sent_bytes`)
- `estimated_bytes` (`int64`, optional): estimated encoded OTLP bytes (`--dry-run` only)
- `skipped_sessions` (`object`): sessions skipped during planning (`already_done`, `failed_previously`, `already_held`)
- `teams` (`array`): per-team breakdown — `{enrollment_id, team_id, team_label, sessions, resume_sessions, records, source_bytes, estimated_bytes?, skipped_sessions}`

Example:
```json
{"type":"plan","holdings_checked":true,"sessions":3,"resume_sessions":1,"records":42,"source_bytes":18432,"skipped_sessions":{"already_done":2,"already_held":1},"teams":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","sessions":3,"resume_sessions":1,"records":42,"source_bytes":18432,"skipped_sessions":{"already_done":2,"already_held":1}}]}
```

### `session_start`

Emitted immediately before streaming a planned session-team upload.

Fields:
- `type`: `"session_start"`
- `index` (`int`): 1-based position in `plan.sessions`
- `total` (`int`): equals `plan.sessions`
- `enrollment_id` (`string`), `team_id` (`string`), `team_label` (`string`)
- `source_system` (`string`), `session_id` (`string`)
- `source_bytes` (`int64`): size of this session's transcript files on disk (`0` if unreadable)

Example:
```json
{"type":"session_start","index":1,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"11111111-1111-1111-1111-111111111111","source_bytes":6144}
```

### `session_done`

Emitted when a planned session-team upload finishes successfully.

Fields:
- All fields of `session_start`
- `records` (`int`): OTLP records sent
- `sent_bytes` (`int64`): encoded OTLP request bytes sent (reconciles with `summary.sent_bytes`)

Example:
```json
{"type":"session_done","index":1,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"11111111-1111-1111-1111-111111111111","source_bytes":6144,"records":14,"sent_bytes":2910}
```

### `session_failed`

Emitted when the server permanently rejects a session (for example HTTP 400 or partial rejection). The failure is recorded on disk and skipped as `failed_previously` on later runs unless `--retry-failed` is passed.

Fields:
- All fields of `session_start`
- `reason` (`string`): server rejection message

Example:
```json
{"type":"session_failed","index":2,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"22222222-2222-2222-2222-222222222222","source_bytes":5120,"reason":"HTTP 400: payload rejected"}
```

### `session_skipped`

Emitted when a planned session is skipped during the upload phase. May occur **without** a preceding `session_start`.

Fields:
- All fields of `session_start`
- `reason` (`string`): canonical skip reason (e.g. `active`, `empty_after_filter`, `missing_file`, `mixed`, `unreadable`, `record_too_large`)

Example:
```json
{"type":"session_skipped","index":3,"total":3,"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"33333333-3333-3333-3333-333333333333","source_bytes":7168,"reason":"active"}
```

### `team_blocked`

Emitted when a team cannot be uploaded to during discovery, planning, or upload. May occur **without** a preceding `session_start`.

Fields:
- `type`: `"team_blocked"`
- `enrollment_id` (`string`), `team_id` (`string`), `team_label` (`string`)
- `reason_code` (`string`): canonical skip reason (e.g. `team_blocked:suspended`, `holdings_check_failed`, `org_access_lost`, `send_failed`, `deselected`, `policy_changed`)
- `reason` (`string`): human-readable reason
- `skipped` (`int`): count of sessions left for a later run (may be `0`)

Example:
```json
{"type":"team_blocked","enrollment_id":"enr-b","team_id":"team-b","team_label":"Acme Labs","reason_code":"org_access_lost","reason":"organization is not accessible","skipped":2}
```

### `summary`

Carries the exact `Summary` object printed by `terma history upload -o json`.

Fields:
- `type`: `"summary"`
- `summary` (`object`): `{dry_run?, interrupted?, discovered_files, sent_sessions, sent_records, sent_bytes, planned?, skipped_sessions?, skipped_segments?, withheld_records?, failed?, team_issues?, unreadable?}`

Example:
```json
{"type":"summary","summary":{"discovered_files":12,"sent_sessions":1,"sent_records":14,"sent_bytes":2910,"planned":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","sessions":3,"records":42,"estimated_bytes":0}],"skipped_sessions":{"active":2,"already_done":2,"already_held":1,"outside_since":3},"failed":[{"enrollment_id":"enr-a","team_id":"team-a","team_label":"Acme Core","source_system":"claude","session_id":"22222222-2222-2222-2222-222222222222","reason":"HTTP 400: payload rejected","failed_at":"2026-11-27T12:00:05Z"}]}}
```

### `error`

Always the last line when present. Indicates a fatal error that prevented the run from finishing.

Fields:
- `type`: `"error"`
- `error` (`object`): `{code, message}`

Example:
```json
{"type":"error","error":{"code":"signed_out","message":"not signed in: signed out during upload"}}
```

## Cancel and Resume Contract

- **Cancelling a run:** Send `SIGINT` (preferred) or `SIGTERM` to the `terma` process and **keep draining stdout until EOF**.
  - `terma` finishes or cancels the in-flight request, persists checkpoint state for any completed chunks, releases the profile upload lock, emits a final `summary` line with `interrupted: true`, and exits `130` (`SIGINT`) or `143` (`SIGTERM`).
  - Even if the reader stops draining stdout before cancelling, `terma` aborts blocked stdout writes after cancellation, releases the upload lock, and exits promptly.
- **Resuming a run:** Run the same `terma history upload --json-stream ...` command again.
  - Completed sessions are recorded in per-route resume state and skipped in `plan.skipped_sessions.already_done`.
  - Partially uploaded sessions are resumed first and counted in `plan.resume_sessions`.
  - Wait for the previous process to exit (or handle `upload_running` / exit `4`) before starting a new run.
- **Handling `signed_out` (exit `6`) and `not_set_up` (exit `5`):** Prompt the user to complete sign-in / setup before re-running `terma history upload`.
