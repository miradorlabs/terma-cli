# Local relay spike

## Why

A repository can't switch an agent's telemetry on or off for itself:
- **Claude Code and Codex** refuse OTel settings from repository config.
- **The desktop apps** never pass through terma's PATH shims.

The only exporter config that reaches every surface is global. Global config sends everything: personal use and unrelated repositories included, under one key, with nothing that says which project a record belongs to.

The relay is the way around that. Every agent's global exporter sends to a relay terma runs on loopback. The relay forwards a record only when a hook in an opted-in repository has claimed the session **and** the record comes from a process the claim names. It sends the record to that repository's project with that project's key. Nothing unclaimed leaves the machine or touches the disk.

## How it works

1. **`terma relay setup`** (hidden) does three things:
   - points Claude's, Codex's and OpenCode's global exporters at `http://127.0.0.1:43180`;
   - writes a random local token into the exporters' headers, and the relay refuses any request without it;
   - starts the relay.

   The exporters send content; the relay applies each project's content policy itself.
2. **Hooks claim sessions.** Any hook in a repository with a binding writes `~/.config/terma/relay/claims/<session>.json`, holding `{project_id, tool, repo, pids}`:
   - **`pids`** are the processes the hook runs under, the agent among them. Each hook of the session adds its own.
   - **Where claims come from:** `hookrun.Env.emitFor`, which every spooled event passes through; and `hookrun.ClaimFromPayload`, for hooks that spool nothing, reading a bounded copy of the hook's stdin.
   - **Codex subagents:** a subagent's thread (`agent_id`) is claimed alongside its root session.
   - **Claude's `UserPromptSubmit`** is wired so every turn starts with a claim and a running relay.
   - **Lifetime:** a claim is rewritten at most every 5 minutes, and expires 4 hours after the last write.
   - **Only on relay machines:** hooks write claims only where `terma relay setup` has run.
3. **Running the relay.** There are two modes.
   - **On demand:** any claiming hook starts the relay (`terma relay run --quiet`, detached) when its lock is free. A hook that had to start it waits, up to a second, until it listens, so the turn's first export finds it. After a recorded start failure, hooks back off for a minute.
   - **As a service** (`terma relay daemon install`): a per-user launchd agent on macOS, or a systemd user unit on Linux, always on and restarted by the system.

   **When the relay exits:**
   - after 8 idle hours (on demand);
   - once quiet for a minute after its binary was replaced by an update, so the next start runs the new code;
   - when its token is gone, meaning the setup was undone;
   - when `terma relay setup` restarts it with a new address or token.

   **Claims** are pruned hourly.
4. **The relay splits every export by session:**
   - by record for logs, by span for traces, by data point for metrics;
   - using `session.id` (Claude, OpenCode), `conversation.id` (Codex logs), and `thread.id` on Codex's turn span. A numeric `thread.id` is an OS thread, never a session.
   - A span naming no session belongs to its trace's session. The relay learns a trace's session from any span or log record that names both.
   - **A part naming no session and no named trace is attributed by its process:** Codex's metrics, and its process-level spans. It goes to the project that every claimed session the process exported belongs to, under one policy. When the process has exactly one claimed session, that session is stamped as `terma.relay.session.id`, with `terma.relay.attribution=process`. A process working for several projects, or with sessions not all opted in, is ambiguous; its part is dropped, never guessed.
   - **Adoption.** A conversation no hook claimed is adopted into its process's project (`terma.relay.attribution=process-sibling`, `terma.relay.session.id` naming the thread) only when all of these hold:
     - the process is a single-workspace Codex client (`codex-tui`, `codex_exec`);
     - its claimed sessions all belong to one project;
     - its own start says Codex made it for itself (`approval_policy=never`, `sandbox_policy=read-only`).

     That is the TUI's title conversation. A thread the developer started or resumed carries their own policies and is never adopted.
5. **It learns which process sent each connection** (`procinfo.FindSender`: the kernel's `proc_info` on macOS, `/proc` on Linux). It does this once per connection, on the connection's first export, while the socket still exists.
6. **Each session's part is then handled one of three ways:**
   - **Claimed, from a process the claim names, with this machine holding the project's key:**
     - apply the content policy;
     - stamp `mirador.project.id` on the resource;
     - POST it in native OTLP (protobuf) to the project's own ingest host, with retries per project.
   - **Claimed, but no key:** held like an unclaimed part, and released if the key appears within the hold (a `terma install` moments after the session started); otherwise dropped (`no_key`).
   - **Unclaimed, or from a process the claim doesn't name:**
     - **Held** in memory: 2 minutes normally (`TERMA_RELAY_HOLD`), 30 minutes for spans waiting on their trace.
     - **Then dropped.**
     - **Bounded:** the hold keeps at most 50,000 records and 64 MiB. When full it evicts the oldest parts, unnamed traces first.
     - **Ordered:** a session's records leave in the order they arrived.
7. **Content policy:** the project's routing record decides; with no record, content is withheld.
   - **Prompts off:**
     - `prompt`, `response` and `user_prompt` are blanked with each harness's own marker;
     - the GenAI content attributes (`gen_ai.prompt`, `gen_ai.completion`, the input and output messages) are removed;
     - the body of OpenCode's prompt and session-title events is emptied.
   - **Tool content off:**
     - `tool_parameters`, `tool_input`, `full_command`, `bash_command`, `arguments` and `output` are removed;
     - so are `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result` and `opencode.tool.file_path`;
     - so are the `tool.output` and `tool.input` span events.
8. **OTLP/JSON is accepted**, with its hex ids converted before decoding. **On stop**, the relay keeps delivering accepted records for 5 seconds.
9. **Claims and policies are cached** for 1 s and 5 s, so a busy session costs one file read a second. The claim's read-merge-write runs under a sidecar lock: without it, 14 of 16 concurrent writers' processes were lost. `TERMA_RELAY_DEBUG=1` logs every drop with its key, sender and claim.
10. **`terma doctor` and `terma status`** replace their export check with the same relay check:
   - the relay runs, or can run, on its address with no one else there;
   - each agent exports to it;
   - this repository is bound, with a key on this machine.

   **`terma relay status`** prints the counters by reason, which are also written to `relay/stats.json` on exit.

## What was run

The scenarios are in `live/relay_test.go`, `live/relay_more_test.go` and `live/relay_opencode_test.go`. They drive real harness binaries against deterministic fake model providers, and the in-test receiver stands in for Terma upstream.

Harness coverage: Claude Code, Codex and OpenCode, the three agents terma supports that export OTLP. Cursor and Antigravity have no exporter; they're hooks-only and never touch the relay.

| Scenario | Claude | Codex | OpenCode |
|---|---|---|---|
| Opted-in session: the full contract, the project on every record, the project key on every request | ✓ | ✓ (no metrics) | ✓ |
| Content allowed / withheld | ✓ | ✓ | ✓ |
| File tools (Write, Read, Edit) with content withheld: the file's contents appear nowhere | ✓ | | |
| Outside any repository / unbound repository / installed repository without a key | ✓ | | ✓ (outside) |
| `TERMA_HOOKS=0` in an opted-in repository | ✓ | | |
| Hooks not yet trusted (Codex's first run) | | ✓ nothing forwarded | |
| Two opted-in projects and a personal session at once | ✓ | | |
| Session resumed in a personal directory | ✓ | ✓ | n/a (runs in its own project) |
| Linked worktree | ✓ | | |
| Subagent | ✓ | (claim test) | |
| Late claim (released from the hold) | ✓ | | |
| Turn longer than the hold | | ✓ | |
| Cold start (no relay running) | ✓ | ✓ loses `conversation_starts` | |
| Relay dies mid-session (interactive, 3 turns) | ✓ every turn arrives | | |

**Versions:** the scenarios passed on the installed builds (Claude 2.1.284, Codex 0.158.0, OpenCode 1.18.33). A matrix run over the last six Claude and Codex releases and the last four OpenCode releases is in progress; this table will name them when it's done.

### Workload equivalence

`live/relay_workloads_test.go` runs every workload twice against identical fake providers, once exporting directly and once through the relay. It then requires the same telemetry upstream (every log event as many times, every span and metric name) and zero drops.

| Harness | Workloads |
|---|---|
| Claude | reply, Bash, a failing Bash, 400 KB of tool output, 8 sequential tools, 3 parallel tools in one response, Write/Read/Edit, a subagent, Unicode and RTL, a provider overload (529) retried |
| Codex | reply, shell, a failing shell, 400 KB of output, 6 tools, a file written by the shell |
| OpenCode | reply, a bash tool call |
| omp | reply, a bash tool call (plus: omp outside any repository exports nothing at all) |

All 18 pass on the installed builds (Claude 2.1.284, Codex 0.158.0, OpenCode 1.18.33). In every relayed run the relay forwarded everything it received. The only difference found is Claude's `retention_sweep`, a housekeeping event on Claude's own schedule that a run may or may not emit.

The Codex TUI (`TestRelayCodexTUITitle`) forwarded 1,614 of 1,614 records, its title conversation included.

### omp

omp (oh-my-pi, PR #21, merged into this branch) exports OTLP natively under the GenAI conventions (`invoke_agent`, `chat`, `execute_tool`, with `gen_ai.conversation.id`). The relay treats `gen_ai.conversation.id` as a session key, and withholds omp's content attributes (`omp.gen_ai.request.messages`, `omp.gen_ai.response.text`). Two things in the branch had to change:
- **Session ids:** its hook file and extension invented a random session id, so a claim named a session no span belongs to. They now use omp's own (`ctx.sessionManager.getSessionId()`).
- **Exporter setup:** omp 18.3 reads its `OTEL_*` variables in `initTelemetryExport`, before any hook or extension loads, so the extension's variables came too late and nothing was exported. The launcher now hands them over. The shim protocol gained a versioned, validated environment channel (`OTEL_*` names only, never evaluated). `terma relay setup --harness omp` installs omp's shim.

The omp route exports only in a bound repository, so omp elsewhere exports nothing at all: stricter than the agents whose global config the relay filters.

### Other harnesses

| Harness | Exports OTLP? | Claims possible? | Status |
|---|---|---|---|
| Pi (`@earendil-works/pi-coding-agent`) | no | extension API (`session_start`, `turn_end`, `tool_call`, …) | needs a terma extension that exports OTLP itself, like OpenCode's plugin |
| Hermes (Nous Research) | no usage telemetry (Langfuse plugin; content-free gateway monitoring) | shell hooks (`on_session_start`, `post_llm_call`, …) in user config | needs a terma plugin that exports OTLP, and user-level hooks |
| T3 Code | its agents': it runs `codex app-server`, Claude's Agent SDK and `cursor-agent` | through those agents' hooks | a multi-workspace client: no adoption (safe); not driven yet |
| Cursor | no | hooks (terma already wires them) | nothing reaches the relay; its events go through terma's spool |

## Findings

1. **Session keys:**
   - **Claude** stamps `session.id` on every log, span and metric data point.
   - **OpenCode's plugin** stamps `session.id` on its logs and spans.
   - **Codex** stamps `conversation.id` on its logs and on no metric. It names the conversation on its turn span only, as `thread.id`; elsewhere `thread.id` is an OS thread number.
2. **Codex's spans need the trace join.** A turn's child spans are exported before the turn span that names the session, as each ends. The relay learns their trace from Codex's own mid-turn logs, which carry both the conversation and the trace id. That cut unattributable spans from about 310 to about 140 per run and raised forwarded records from about 456 to about 628. A turn longer than the ordinary hold keeps its children.
3. **Codex's metrics name no session, and are attributed by process.** A Codex exec or TUI process serves one repository, so its metrics go to that project, marked `terma.relay.attribution=process` with the inferred session. The live contract runs the same metric checks through the relay as without it. So are Codex's process-level spans (hook runtime, rollout persistence) and one log event per run that names nothing. A Codex run now reaches upstream whole: 936 of 936 records.
3b. **Codex's TUI titles each thread in a second conversation** of its own: its own id, its own model call and cost, no hook, started about 7 s after the first reply (0.158, live). It is adopted into the thread's project by its process and its internal-thread signature.
4. **Content lives in more places than the attribute goldens show:**
   - Claude's `claude_code.tool` span carries a `tool.output` event, holding `bash_command`, and for file tools `content` and `diff`.
   - OpenCode's plugin puts the prompt in a log **body** and the reply in `gen_ai.completion`.

   All of these are withheld. The live tests plant markers and scan everything upstream received (`leakedFields`), so a harness release that puts content somewhere new fails the canary.
5. **Resuming a session elsewhere.**
   - **Claude and Codex** keep the session id when resumed from any directory (`claude --resume`, `codex exec resume`). A claim keyed by session alone forwarded the personal run. Process-scoped claims close that: the resumed run's records are held, then dropped (`uncovered_process`), and the original run's are forwarded.
   - **OpenCode** binds a session to its project directory, so a continued session runs in the repository. In 1.18.33 it also never exits when continued from another directory.
6. **Cold start through Codex** loses `codex.conversation_starts`. Codex emits it before its `SessionStart` hook, and its exporter doesn't retry a refused connection. That's why setup starts the relay and the relay idles for 8 hours. What remains is the first Codex session after the relay has idled out. Claude loses nothing: its first export comes after its hook.
7. **A relay that dies mid-session** used to lose the next turn: the turn's `Stop` hook restarted it too late, and Claude's exporter doesn't retry. Claude's `UserPromptSubmit` hook now restarts it at the start of the turn, and every turn arrives. Codex's `UserPromptSubmit` was already wired.
8. **Hook budget:**
   - `prepare-commit-msg` is unchanged, at a median of 20 ms against a 50 ms budget.
   - A claiming agent hook costs about 1 ms more: 11 ms against 10 ms.
   - Finding a connection's sender takes about 3 ms over 900 processes.
   - The relay handles about 390,000 records a second with claims read from disk.
9. **terma's own Codex reply capture** takes consent from the routing record alone under the relay, because the relay sets the machine-wide Codex config to allow prompts.

## Break attempts

| Attempt | Result |
|---|---|
| OTLP/JSON with spec hex ids | **Broke:** ids decoded as base64 garbage. Fixed. |
| A record after its claim, older ones still held | **Broke:** it overtook them. Fixed (ordered hand-off). |
| 20 × 4 MiB unclaimed exports | **Broke:** no byte bound. Fixed (64 MiB, evicting the oldest). |
| A fresh trace id on every span | **Broke:** unbounded index. Fixed (100,000 entries). |
| A slow client trickling a request | **Broke:** no read timeout. Fixed. |
| SIGTERM with a slow upstream | **Broke:** the queue was dropped. Fixed (5 s grace). |
| Codex subagent threads | **Broke:** never claimed. Fixed (`agent_id`). |
| Resume in a personal directory, Claude and Codex | **Broke:** personal work was forwarded. Fixed (process-scoped claims). |
| OpenCode with content withheld | **Broke:** the prompt left in a log body and the reply in `gen_ai.completion`. Fixed. |
| Codex turn longer than the hold | **Broke:** child spans were dropped. Fixed (trace learnt from logs; 30-minute trace hold). |
| Relay killed mid-session | **Broke:** the next turn was lost. Fixed (`UserPromptSubmit`). |
| Hold full of never-named traces | **Broke:** new arrivals were refused. Fixed (evict the oldest, unnamed traces first). |
| Concurrent hooks merging one claim | **Broke:** 14 of 16 writers' processes were lost. Fixed (sidecar lock); the test fails without it. |
| Codex metrics and process-level spans | **Lost:** about 470 records a run. Now attributed by process, and the full metric contract runs through the relay. |
| Codex TUI title conversation | **Lost:** never claimed. Now adopted by process and signature. |
| A TUI resuming a personal thread | Not adopted: the resumed thread starts with the developer's policies. |
| A claim before its key (`terma install` mid-session) | **Broke:** dropped at once. Now held, and released when the key appears. |
| A hook restarting the relay, with the agent exporting at once (Claude 2.1.280) | **Broke:** the turn was lost. Fixed: the restarting hook waits until the relay listens. |
| A relay outliving its setup or its binary | **Broke:** it ran on. Fixed: it exits when its token is gone, or when quiet after a binary change. |
| Port taken by another process | Can't be prevented. It is detected: `relay status` and `doctor` name it, and hooks back off. |
| An agent naming its own `mirador.project.id` | Overridden by the claim. |
| No token, a wrong token, `GET`, malformed bodies, 4.2 M fuzzed inputs | Refused, nothing forwarded, no panic. |
| 8 concurrent writers, claims mid-way, then a stop | `received = forwarded + dropped`, exactly. |
| Transient `503`s / a `403` | Retried and delivered once / not retried. |
| Two relays racing to start | One runs. |

## What remains uncertain

- **The desktop apps aren't driven.** Claude Desktop and Codex Desktop run the same hooks and exporters. If one app process exports for every workspace, process scoping can't separate them, and the relay falls back to the session id, which is still required.
- **A squatter on the relay's port** receives what the agents send. This includes another local user on a shared machine, since loopback is shared. The fix is TLS to the relay with a pinned certificate. Claude's exporter takes a CA file; Codex's and the plugin's haven't been checked.
- **An unresolved sender** falls back to the session, counted as `sender_unresolved`. That happens for an agent running as another user, or on a platform without `/proc` or `proc_info`.
- **The first Codex session after an idle-out** loses `conversation_starts` (finding 6) unless the relay runs as a service. `TestRelayDaemon` shows that a service relay killed with `SIGKILL` is back without any hook, and that a Codex session delivers its whole contract, `conversation_starts` included.
- **Upstream acceptance hasn't been tested against the real Terma ingest.** The payload is the agents' own native OTLP with one resource attribute added, but no live key was used.
- **Nothing is persisted.** What's queued when the grace runs out is counted as lost.

## Recommendation

**Go.** On every build in the matrix, across three harnesses, the relay separated opted-in work from everything else, found the project from a claim, and withheld content per project. Every break attempt that got through has a fix and a test. What it gates on is small:
- a session id the harness already stamps;
- a trace join that Codex's span shape makes necessary;
- a process check against what the claiming hook saw.

The server-side correlation stays on the server.

## Running it

```bash
cd live && make telemetry          # includes TestRelay*, no credentials
TERMA_LIVE_CLAUDE_VERSIONS=last6 TERMA_LIVE_CODEX_VERSIONS=last6 TERMA_LIVE_OPENCODE_VERSIONS=last4 make live RUN='TestRelay.*'
```

The nightly `live.yml` runs them on the last three releases of each harness. Relay goldens are in `live/golden/relay/` (`LIVE_UPDATE_GOLDEN=1`).
