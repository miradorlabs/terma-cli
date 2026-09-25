# Subagent collection

How terma records the agents a coding session spawns, what it learns about each one, and
what it deliberately does not read.

Every event here is hook evidence, filed under the session a person can find, with the
subagent named on it. The harness's native OTel export remains the usage counter: it
meters the same tokens request by request. What the hooks add is the join — which agent,
on which model, did which part of the work — and for Claude Code how one run went: how
long it took, how many tools it used and what they did. **No hook reports what a run
spent**; see "What a run spent" below.

## Two shapes

**A facet of one session.** Claude Code and Codex run a subagent inside the parent
session. Every hook payload keeps the parent's `session_id` and adds `agent_id` and
`agent_type`. terma files `terma.subagent.start`, `terma.subagent.end` and (Claude Code)
`terma.subagent.call` under the parent's session id, and stamps the same agent facet on
everything else the subagent's hooks produce: `terma.files.touched` for its edits and, in
Codex, the quota and reply capture that run inside its thread. Manifests and commit
trailers stay per session: the parent's commit of a subagent's file is attributed the same
way as its own edits.

`agent_id` is the discriminator. `claude --agent <name>` sends `agent_type` on every hook
of the session, main thread included, so a type without an id stamps nothing.

For Claude lifecycle events, an id alone does not prove a delegated run. Claude's
internal forks (including periodic background-agent summaries, prompt suggestions and
`/btw`) also fire `SubagentStop`, with new ids and no `SubagentStart` or Agent call.
The type is usually empty, but can inherit the session's `--agent` name; filtering
empty types alone misses that case. See the [Claude hook reference](https://code.claude.com/docs/en/hooks#subagentstop).

Terma therefore records launch evidence from `SubagentStart` or a successful
`Agent`/legacy `Task` tool response before accepting `SubagentStop` for the same
`(session_id, agent_id)`. Each pair has a separate atomic marker under
`claude-subagents/` in the config directory, shared across worktrees and hook
processes. Each observed start or Agent/Task response refreshes the marker. Stops
keep it but do not refresh its age, so repeated stops remain eligible within the
same retention window. Markers expire 14 days after the latest launch evidence,
and session starts prune them. A long-running or resumed agent with no new launch
evidence for 14 days will therefore have its final stop omitted, even if it was
active during that time. The retention window bounds local launch state; it is not
an inactivity measurement.
An unobserved launch (including one before an upgrade, a failed marker write or an
expired marker) means its stop is omitted; its launch/tool-call evidence and native
usage remain available. This filters future hook events; previously ingested rows
require a separate platform cleanup.

Checked 2026-09-25 against a live dev session: four agent launches, 107 orphan end
records with no type, and no orphan start records. Forty orphan ends aligned within
one second of native `query_source=agent_summary` calls. The installed Claude Code
2.1.282 bundle confirms a 30-second summary timer, a fresh fork id, no start-hook
dispatch on that path, and the shared stop-hook path. The other orphan ends are not
individually classified; none has evidence of delegation.

**A session of its own.** Cursor and OpenCode give the child a conversation or session of
its own. OpenCode's `terma.session.start` names the parent in `parent_session_id`, and the
child is never made the repository's active session: that is what claims a commit no
manifest accounts for, and it belongs to the session a person is driving. Cursor reports a
finished subagent with `subagentStop`, which terma files under the conversation that
spawned it; a manifest the subagent built under its own conversation id is folded into the
parent's (`session.Store.Merge`), so the commit is stamped once.

## Events

| Event | Session id | Attributes |
|---|---|---|
| `terma.subagent.start` | the parent | `tool`, `agent_id`, `agent_type`, `turn_id` (Claude `prompt_id`, Codex `turn_id`), `model` (Codex), `schema_version: 1`, `terma.version`. Codex adds `rollout_status` and, when it is `present`, the spawn record: `agent_parent_id`, `agent_depth`, `agent_nickname`, `agent_path` |
| `terma.subagent.end` | the parent | `tool`, `agent_id`, `agent_type`, `turn_id`, `model` (Codex). Cursor adds `status` (`completed` / `aborted` / `error` / `unknown`), `duration_ms`, `message_count`, `tool_call_count`, `loop_count`, `file_count`, `evidence_source: cursor_hook`; its `agent_id` is the subagent's conversation id when Cursor sends one |
| `terma.subagent.call` | the parent | Claude Code only. `tool`, `agent_id`, `agent_type`, `model`, `tool_call_id`, `turn_id`, `status` (`async_launched` / `completed` / `unknown`), `is_async`, `agent_parent_id` when a subagent launched it, `schema_version: 1`. When `completed`: `duration_ms`, `tool_call_count`, `final_context_tokens` (the size the subagent's context had reached, **not** what it spent), `service_tier` and `speed` (of its last request), and what its tools did — `read_count`, `search_count`, `bash_count`, `edit_file_count`, `lines_added`, `lines_removed`, `other_tool_count` |
| `terma.files.touched` | the parent | existing attributes plus `agent_id` / `agent_type` when a subagent made the edit; Cursor's `modified_files` arrive with `tool_name: subagentStop` and the turn |
| `terma.session.quota`, `terma.session.capture`, `terma.assistant.message` | the parent | Codex: existing attributes plus `agent_id` / `agent_type` when captured inside a subagent's thread |
| `terma.session.start` | the child | OpenCode: existing attributes plus `parent_session_id`. Codex, if a build ever starts a spawned thread as a session: `parent_session_id`, `agent_depth`, `agent_nickname`, `agent_path` |

Every number is optional, and one the harness did not send stays absent rather than zero:
a subagent launched in the background has no totals, and a zero would read as one that
used nothing. A `status` outside the vocabulary arrives as `unknown`, never as free text.
`agent_id` and `agent_parent_id` are held to a session id's charset; `agent_type`,
`agent_nickname`, `agent_path`, `service_tier` and `speed` are kept single-line and at most
128 bytes.

**Not read, ever:** the task's description and prompt, the subagent's reply
(`tool_response.content`, `last_assistant_message`), its transcript path, Cursor's task and
summary. The Agent tool's response holds the first three beside the numbers terma wants;
`claudeAgentResult` has no field for them, so they are never decoded, and a test plants
sentinels in all three to notice if that changes.

### What a run spent

Nothing here says, and the Agent tool's response does not either — whatever its field
names suggest. `tool_response.usage` is the usage of the subagent's **last API request**,
and `totalTokens` is that one request's four classes added up: the size the subagent's
context had reached when it finished. Two live runs on 2.1.278, each of two requests
(input / cache read / cache write):

| Request 1 | Request 2 | `usage` in the response | `totalTokens` |
|---|---|---|---|
| 10 / 0 / 13071 | 8 / 13071 / 2357 | 8 / 13071 / 2357 | 15572 |
| 10 / 0 / 13060 | 8 / 13060 / 2192 | 8 / 13060 / 2192 | 15385 |

The first request is in neither number. Any subagent that uses a tool makes at least two
requests, so read as a total this understates every real run. The first cut of this
event took it for one, as `total_tokens` plus by-class counts, until a reviewer added the
requests up; it was corrected before release. So:

- `totalTokens` is sent as **`final_context_tokens`**, which is what it is.
- The by-class numbers are **not sent**. They are one request's, and the native export
  already carries that request under its own id; beside a run's duration they read as the
  run's. A test keeps them out.
- `totalDurationMs`, `totalToolUseCount` and `toolStats` *are* totals of the run.

A subagent's spend is the native export's to report: `api_request` log events carry
`query_source=agent:builtin:<Type>` and `agent.name`, which name the agent's **type** and
never which run. Tying spend to one run means joining those model calls to the run's
`[start, end]` window (ambiguous when two agents of a type overlap) or reading `agent_id`
off Claude Code's trace spans, which do carry it. Neither is done yet.

## Per harness

| Harness | Mechanism | Status |
|---|---|---|
| Claude Code | `SubagentStart` / `SubagentStop` hooks (`agent_id`, `agent_type`, `prompt_id`, parent `session_id`); `PostToolUse` inside a subagent carries `agent_id` / `agent_type`; the parent's `PostToolUse` for the `Agent` tool (`Task` in older builds) carries `tool_response.{agentId, agentType, resolvedModel, status, isAsync}` and, when the parent waited, `totalDurationMs`, `totalTokens`, `totalToolUseCount`, `usage`, `toolStats` | Live-verified on Claude Code 2.1.278, 2026-09-21: two `-p` runs, one subagent launched in the background and one in the foreground, every hook payload captured. `SubagentStart` names no model — `resolvedModel` on the Agent call is the only place a hook does |
| Codex | `SubagentStart` / `SubagentStop` in `.codex/hooks.json`. A Codex subagent is a thread the session spawned: `session_id` is the root thread's, `agent_id` the child thread's id, `transcript_path` the child's own rollout, whose first line is the spawn record (`session_meta.payload.source.subagent.thread_spawn.{parent_thread_id, depth, agent_nickname, agent_path}`, read by `harness.CodexRolloutSpawn`). `SubagentStop` fires at the end of **every** turn of the child thread, so a Codex end is per turn, told apart by `turn_id`. Quota and reply capture inside the thread read the child's rollout (`codexRolloutID`) | Source-read against `codex-rs/core/src/hook_runtime.rs`, 2026-09-17, and the spawn record verified against 22 spawned rollouts on a developer machine. Not yet seen live. Codex's source dispatches no `SessionStart` for a spawned thread; terma keeps a one-line read there in case a build does. Needs the developer's per-entry trust like every Codex hook — see below |
| Cursor | `subagentStop` (`subagent_id`, `parent_conversation_id`, `subagent_type`, `status`, counts, `modified_files`), with `loop_limit: null` like `stop`. cursor-agent can file a subagent's own `afterFileEdit` under the subagent's conversation id, which is what the fold is for | Read from the installed cursor-agent bundle, 2026-09-17; not yet live-verified. `subagentStart` is never wired: Cursor documents that a hook printing nothing blocks the subagent, and the committed guard prints nothing on a machine without terma |
| OpenCode | `Session.parentID` on `session.created`, forwarded by the plugin as `parent_session_id`, as the `opencode.parent_session.id` attribute on the `opencode.session.created` log record, and on every `chat` and `execute_tool` span of the child so the link survives a traces-only policy. On an OpenRouter request from the child, the plugin's `chat.params` hook also adds `parent_session_id` and `agent` to the body's `trace` field (keeping any trace keys the developer configured), which OpenRouter's broadcast exports as `trace.metadata.parent_session_id` / `trace.metadata.agent`: the only way a gateway that sees just the requests can place the child, because the parent's task result arrives after the child has finished | SDK type verified; plugin unit-tested (`bun test internal/harness/opencode`). The `trace` field live-verified on opencode 1.18.31 through OpenRouter's broadcast to the dev gateway, 2026-09-25: all three generations of an `explore` child carried the parent and agent, and the parent's generations carried neither |
| Antigravity | Nothing in agy's hook payloads names a subagent. A subagent tool shows up as a `terma.tool.call` with its `tool_name`, like any other step | Not collected; agy 1.2.7 |

### Codex trusts a hook entry by entry

Codex records trust per entry, keyed `<hooks file>:<event>:<group>:<handler>`. A developer
who trusted terma's hooks before `SubagentStart` and `SubagentStop` were added has a file
that counts as trusted and two hooks Codex skips without a word — every subagent in that
repository unrecorded. `terma doctor` compares terma's entries in the file with the ones
Codex has a trusted hash for (`hookmgr.CodexTermaEntries`, `CodexHookTrust.TrustedKeys`) and
names what is skipped; review them in Codex Desktop's Hooks settings or run
`/hooks` in Codex CLI.

## What the OTel export adds (Claude Code)

Observed on 2.1.273: `api_request` log events from a subagent carry
`query_source=agent:builtin:<Type>` and `agent.name`; a `subagent_completed` event carries
`agent_type`, `total_tokens`, `total_tool_uses`, `duration_ms` and `model` (its
`total_tokens` is unverified and, given the above, more likely the same final-context
figure than a run total — check before trusting it); the
`claude_code.token.usage` and `claude_code.cost.usage` metrics split `query_source` into
`main`, `subagent` and `auxiliary` with `agent.name`; trace spans inside the subagent carry
`agent_id`. Subagents share the parent `session.id` throughout. This is where subagent
spend is measured; the hook events above say which agent did what. The spans' `agent_id`
is the one native field that could tie a run's requests to the run.

Also on 2.1.278, not collected: `Stop` and `SubagentStop` list `background_tasks`
(`{id, type: "subagent", status, agent_type}`) — the subagents still running when a turn
ends.

## Platform follow-ups

- The terma-cli log adapter (`gateways/otel/internal/mapping/aisignal/termacli_logs.go`)
  needs to learn `terma.subagent.start`, `terma.subagent.end` and `terma.subagent.call`, and
  the `parent_session_id`, `agent_id`, `agent_type` and `agent_parent_id` attributes. Until
  it does they sit in the raw log store.
- `terma.subagent.call` is deduplicated on `tool_call_id`. It carries no token counts
  (see "What a run spent"); `final_context_tokens` is a size, never a spend, and belongs in
  no sum.
- `parent_session_id` on the AI session summary (backend) is what turns the frontend's
  existing session tree on.
- `query_source` and `agent.name` promoted to typed dimensions on Claude model calls is
  what makes subagent spend a query.
- `https://terma.ai/cli/llms.txt` (terma-frontend) lists the Claude Code hooks terma
  installs and the `PostToolUse` matcher; both changed here.
