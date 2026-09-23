# Antigravity collection

Google's Antigravity CLI (`agy`) replaced Gemini CLI for Pro, Ultra and free Code Assist
users on 18 June 2026. It has no configurable OpenTelemetry export — its single
telemetry switch reports to Google — so everything Terma learns about an agy session
arrives through the lifecycle hooks `terma install` writes into the repository.
Install with `terma install --adapters antigravity`, or let a repository that already
carries one of agy's customization directories (`.agents`, `.agent`, `_agents`,
`_agent`) get them by default.

## What is written

`.agents/hooks.json`, agy's workspace hooks file, keyed by hook *name*. terma owns one
name, `terma`, holding `PreInvocation`, `PostToolUse` (unmatched: every tool step),
`PostInvocation` and `Stop`. Every other author's named hook in the file survives
byte-for-byte; uninstall removes exactly terma's key, and the file itself when nothing
else remains. A developer's `"enabled": false` on terma's entry is preserved by a
reinstall and reported by `terma doctor`. There is no `PreToolUse` entry: agy requires a
decision from that hook and terma never decides anything for an agent.

Each command is the shared guard, `command -v terma >/dev/null 2>&1 && terma hook
<event> || true`, and every handler answers `{}` on stdout, the reply agy documents.
On a machine without terma the guard prints nothing and agy raises no warning (checked
against agy 1.2.4 with a second named hook whose binary was absent).

## When the hooks fire

Two conditions agy never states out loud, both surfaced by `terma doctor`:

- **Trust.** agy loads a workspace's hooks only after the developer has trusted the
  workspace from inside agy. The record is `trustedWorkspaces` in
  `~/.gemini/antigravity-cli/settings.json`; terma only reads it.
- **Workspace binding.** Hooks belong to the *workspace*, not the working directory. An
  `agy` started in a directory that is not one of its projects works inside the default
  "CLI Project" (a scratch folder under `~/.gemini/antigravity-cli/`) and loads no
  repository hooks at all. In print mode, `--add-dir <repo>` binds the turn to the
  repository; interactively, opening the folder as a project does.

Hooks run synchronously from `<workspace>/.agents` with agy's default 30-second
timeout (terma asks for 10) and `ANTIGRAVITY_CONVERSATION_ID` in the environment.

## What is captured

Payload keys are protojson camelCase. Every event carries `conversationId`,
`workspacePaths`, `modelName`, `transcriptPath` and `artifactDirectoryPath`; terma
reads the first three and never opens the transcript or the artifact directory.

| Hook | terma event | Notes |
|---|---|---|
| `PreInvocation` (`invocationNum` 0, `initialNumSteps` ≤ 1) | `terma.session.start` | A fresh conversation's first model call. Later turns and resumed conversations refresh the active session without a new start. |
| `PreInvocation` (`invocationNum` 0) | `terma.session.observation` | The start of a turn: `model`, `invocation_num`, `initial_num_steps`, `turn_id`. Invocations in the middle of a turn are not observed here (`PostInvocation` reports them). |
| `PostToolUse`, every step | `terma.tool.call` | `tool_name` (`toolCall.name`), `tool_call_id` (`step-<stepIdx>`), `step_idx`, `turn_id`, `model`, `status` completed/error. No `duration_ms`: agy reports none. See "Tool calls and turns". |
| `PostToolUse` for an edit tool | `terma.files.touched` | `toolCall.name` ∈ write_to_file, replace_file_content, multi_replace_file_content, create_file, edit_file, delete_file, str_replace_editor (not `view`), notebook_edit; the path is `TargetFile`, `target_file`, `file_path`, `path` or `notebook_path`. Reads and tools outside the repository are ignored. Carries the step's `tool_call_id`, `step_idx` and `turn_id`: it is what that call changed, not a second call. |
| `PostInvocation` | `terma.session.observation` | `model`, `invocation_num`, `initial_num_steps`, `turn_id`, one per model call. |
| `Stop` | `terma.session.observation` | `model`, `execution_num`, `turn_id`, `termination_reason` (agy's own string, e.g. `NO_TOOL_CALL`), `fully_idle`, `status` ok/error (presence only; the error text never travels). Starts a detached spool flush. |

Observations share the ordering contract of Cursor's: `source_stream`,
`observation_sequence` and `observation_id` give durable per-conversation order and
replay identity from a write-ahead checkpoint under `antigravity-observations/` in the
Terma config directory; adjacent identical snapshots are suppressed for ten minutes;
`ordering` is `local_receipt`. `usage_status`, `funding_status`, `quota_status` and
`account_status` are all `unavailable`: **the hook payloads carry no token counts, and
neither do agy's transcripts** (`brain/<conversation>/.system_generated/logs/
transcript*.jsonl` hold steps, tool calls and content, nothing about usage). Print
mode's `--output-format stream-json` reports `usage` per response on stdout, but that
stream is agy's caller's, not a hook's. Treat these observations as evidence of
activity, never as usage or spend.

The commit trailer is `Agent-Tool: antigravity`; the session id is agy's conversation
id. There is no session-end hook: the active session ages out after `ActiveTTL`.

## Tool calls and turns

agy has no other export, so its `PostToolUse` hook — unmatched, it fires for every tool
step — is the only record of what a session *did*. Each step is one `terma.tool.call`,
the event Cursor's tool calls use. Reads, searches and shell commands are reported as
well as edits: a session that only investigated used to look like one that did nothing.

- **Identity.** agy issues no call id. `stepIdx` is the step's index in the conversation's
  trajectory; it only grows, across turns and across a resumed conversation alike, so
  `tool_call_id` is `step-<stepIdx>` and a redelivered event is the same event.
- **Outcome.** `status` is `error` when the payload's `error` is set and `completed`
  otherwise — agy's reading, not terma's: a shell command that exits non-zero is a
  completed step. The error text never travels. There is no duration.
- **Arguments.** `toolCall.args` is opened for one thing, an edit tool's path. A command
  line, a search query or a file's content is never read.
- **Turns.** agy names no turn, and nothing in a payload identifies one: `invocationNum`
  restarts at 0 every turn, `initialNumSteps` moves with every *invocation*, and Stop's
  `executionNum` counts executions of the process — it was `0` on both turns of one
  resumed conversation. What identifies a turn is where it began. `turn_id` is
  `turn-<initialNumSteps>` of the turn's invocation 0: the length of the trajectory when
  the person's message arrived. `PreInvocation` records it under `antigravity-turns/` in
  the Terma config directory and the turn's later hooks — each its own process — read it
  back. A step or a Stop whose turn terma did not see begin carries no `turn_id`.

## Verification

Live on 2026-09-18 with agy 1.2.7 (darwin/arm64), print mode, two turns of one
conversation (`--conversation <id>` for the second): the payload keys are those of 1.2.4;
`stepIdx` ran 2, 4, 6 in the first turn and 11, 13 in the second; `invocationNum` restarted
at 0 and `initialNumSteps` read 1, 3, 5, 7 then 9, 12, 14; `executionNum` was 0 at both
Stops; and `ls` on a missing directory (exit 1) arrived with an empty `error`.

Live on 2026-09-16 with agy 1.2.4 (darwin/arm64), print mode, `--add-dir` binding a
scratch repository added to `trustedWorkspaces`: one turn creating and editing a file
produced one `terma.session.start`, two `terma.files.touched` (write_to_file,
replace_file_content), three `PostInvocation` and one `Stop` observation, a manifest
with the file, a stamped commit and its `terma.commit` record. `view_file` produced no
attribution. The colleague-without-terma hook produced no agy warning. `terma doctor`
reported the untrusted workspace once the trust entry was removed. The recorded
payloads are the fixtures in `internal/hookrun/antigravity_test.go`.

Interactive agy and the Antigravity IDE were not exercised. The IDE shares the
customization layout but its hooks support and directory names (`antigravity-ide/`)
are unverified here.

## Sources

- agy's bundled hooks guide (`~/.gemini/antigravity-cli/builtin/skills/agy-customizations/docs/hooks.md`, agy 1.2.4): file format, event set, matcher semantics, input/output contract, trust requirement.
- Recorded hook payloads and agy's own `cli.log` (`hooks_manager: loaded N named hooks`).
- [Gemini CLI retirement](https://www.digitalapplied.com/blog/gemini-cli-to-antigravity-cli-migration-june-18-2026-guide): Pro/Ultra/free Code Assist users lost Gemini CLI on 18 June 2026; Code Assist Standard/Enterprise and paid API keys kept it.
