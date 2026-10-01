# Local relay

## Why

A repository can't switch an agent's telemetry on or off for itself:
- **Claude Code and Codex** refuse OTel settings from repository config.
- **The desktop apps** never pass through terma's PATH shims.

The only exporter config that reaches every surface is global. Global config sends everything: personal use and unrelated repositories included, under one key, with nothing that says which project a record belongs to.

The relay is the way around that. Every agent's global exporter sends to a relay terma runs on loopback. The relay forwards a record only when a hook in an opted-in repository has claimed the session **and** the record comes from a process the claim names. It sends the record to that repository's project with that project's key. Nothing unclaimed leaves the machine or touches the disk.

## What was run

The scenarios are in `test/live/relay_test.go`, `test/live/relay_more_test.go` and `test/live/relay_opencode_test.go`. They drive real harness binaries against deterministic fake model providers, and the in-test receiver stands in for Terma upstream.

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

**Versions:** the matrix ran every scenario above, plus the workloads and the direct telemetry contracts, from frozen binaries (578 runs):
- **Claude Code:** 2.1.280–2.1.285.
- **Codex:** 0.156.0–0.159.0, with 0.159.1 in part as it came out during the run.
- **OpenCode:** 1.18.30–1.18.33.

Every relay scenario passed on every build. Two failures came from elsewhere:
- **`TestRelayWorkloadsCodex`:** on some runs the relayed `codex exec` exited without its shutdown. That is Codex's known exit without `SessionEnd` (the `TERMA_LIVE_KNOWN_UPSTREAM` note in `test/live/Makefile`), and with it the shutdown's own telemetry never left the process (`session_loop`, `op.dispatch.shutdown`, the last metrics flush). It is not the relay: across five runs each, direct exec shut down cleanly 2 times and relayed exec 3 times. The comparison now notes what an unshut run never sent. It still requires that the relay dropped nothing and forwarded everything it received.
- **`TestClaudeTelemetry/redacted`:** fails on 2.1.280 and 2.1.281 with a terma built from `main` too. Claude exported the prompt with prompts off; 2.1.282 and later do not. The relay's own withheld-content scenario passed on those builds, because the relay removes content itself, whatever the harness's switches say.

Later additions, run on the installed builds and the last four Codex releases:
- Claude Desktop, on its pinned 2.1.202 and on the newest build;
- Codex's app-server and daemon, on 0.157.0–0.159.1.

### Codex's app-server (Desktop, the daemon)

Codex Desktop, the IDE extension and, since 0.157, the interactive TUI run their threads in `codex app-server`. A bare `codex` with a daemon running attaches to it (`daemon_auto_start`, stable and on). Any `-c` override or `--dangerously-bypass-hook-trust` runs the TUI in-process instead; so do `codex exec` and terma's own Codex shim.

The daemon is one process per `CODEX_HOME`, reached at `$CODEX_HOME/app-server-control/app-server-control.sock`. It serves every workspace, and it spawns every thread's hooks and exports all their telemetry (`service.name=codex-app-server`). The TUI process exports only a few metrics of its own, which name no session. What that means for the relay:

- **Process scoping separates nothing within the daemon:** every claim names the daemon, so only `conversation.id` tells threads apart. A loaded thread keeps its own working directory: `thread/resume` with another `cwd` is ignored, and the resumed turn runs in the repository with its hooks. A daemon or Desktop restart unloads every thread. A thread resumed from a personal directory after that runs in a new process, and process scoping holds it back (`TestRelayCodexDesktopResumedElsewhere`: 412 records dropped as `uncovered_process`).
- **Originator names the first client, not the thread's:** it is global to the daemon and set by the first client's `clientInfo.name` (a TUI connecting after Desktop reports `Codex_Desktop`). The relay keeps every client a process served and adopts only when all of them are single-workspace. What actually prevents a wrong adoption is evidence: a process with any unclaimed thread the developer started is ambiguous. The relay learns every part of an export before it routes any, so a personal thread's records and its title's in the same batch make the process ambiguous first. The daemon test covers this: under sabotage the personal title was adopted and the test failed.
- **The thread's own work is named by `session_loop`:** its internal spans (turn context, rollout persistence, its hook commands, shutdown) sit under the `session_loop` root span, which names the thread as `thread_id` (underscore). That is now a session key, numeric values excluded. `session_loop` is exported when the loop ends, so these spans wait in the trace hold until then.
- **A thread's start waits for its first turn:** app-server exports `conversation_starts` at `thread/start`, and the first hook fires with the first turn, whenever the developer types. An unclaimed conversation start now waits as long as a trace (30 minutes), not 2 minutes. `TestRelayCodexDesktop` waits past the test's hold before the first turn, and the start still arrives.
- **Metrics:** the app-server exported no OTLP metrics in any run, 0.157.0 through 0.159.1, including after a clean SIGTERM. The only session-less counters seen come from the TUI client processes, and those name no session and are dropped.
- **The exporter is read once:** the daemon reads `[otel]` when it starts. A change reaches it only after `codex app-server daemon restart`, and Codex warns that running work may be interrupted. `relay setup` says so when a daemon predates it, and doctor warns until the daemon has restarted (read from `$CODEX_HOME/app-server-daemon/daemon.pid`). terma never restarts it.

Tests (`test/live/codex_appserver.go` drives app-server over stdio JSON-RPC the way Desktop does, and a sandbox daemon with a TUI attached to it):
- **`TestRelayCodexDesktop`:** one process holds a repository thread and a personal thread. The first reaches its project, the second nothing.
- **`TestRelayCodexDesktopResumedElsewhere`:** a thread is resumed from a personal directory after a restart, and nothing of the resumed turn leaves.
- **`TestRelayCodexDaemonTUI`:** a TUI attached to the daemon, verified by the claim naming the daemon's pid. The repository thread reaches the project; its title conversation, a personal thread in the same daemon and that thread's title reach nothing.

All three pass on 0.157.0 through 0.159.1. The daemon test needs a short `CODEX_HOME`, because a socket path is limited to 104 bytes.

### Claude Desktop

Claude Desktop's Code tab never runs the `claude` on PATH, so terma's per-repository shim never reaches it. What reaches it is the user's `settings.json`, which is where `relay setup` points the exporter. Read from the app bundle (Desktop 1.19367.0):
- **Binary:** its own pinned Claude Code (2.1.202), under `~/Library/Application Support/Claude/claude-code/<version>/`.
- **Launch:** through the Agent SDK, under the `Contents/Helpers/disclaimer` helper, which stays alive as the parent (Claude → disclaimer → claude).
- **Transport:** `stream-json` over pipes, with no terminal, so the status line never renders.
- **Settings:** `--setting-sources=user,project,local`, so the user's exporter settings and the repository's hooks both apply.
- **Service name:** `service.name=claude-code-desktop`, set through `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`. The relay routes by session, never by service name. The backend's adapter must accept the Desktop name.
- **Worktree mode:** "use worktree" creates linked worktrees at `<repo>/.claude/worktrees/<name>`, which find the main checkout's binding.

`TestRelayClaudeDesktop` (`test/live/claude_desktop.go`) reproduces that launch on the pinned build and on the newest. On one relay it runs a repository session, a worktree session and a personal session. The first two reach the project with its key, and their hooks ran. The personal session was received and dropped. It passes on 2.1.202 and 2.1.284.

**Not covered:** Desktop's cowork mode (`claude-code-vm`) runs Claude Code in a Linux VM:
- with user settings only (so no repository hooks, and no claims);
- with its own `CLAUDE_CONFIG_DIR`;
- behind a userspace network where 127.0.0.1 is not the host.

Nothing it does reaches the relay, so it is lost rather than leaked.

### Tools must not inherit the relay

An agent's tools must never inherit the relay's `OTEL_*` variables, since `OTEL_EXPORTER_OTLP_HEADERS` carries its token. If they did:
- an OTel-instrumented program under test would export to the relay (and be dropped), not to its own collector;
- cursor-agent merges `OTEL_EXPORTER_OTLP_HEADERS` into what it sends Cursor's backend.

Where each agent stands:
- **Claude Code:** strips them from its tools (`TestRelayClaudeToolsGetNoExporter`, on 2.1.202 and 2.1.284).

## What still reads files on disk

The relay's own attribution reads no harness files: everything it knows comes from OTLP and hook payloads. Some hook features from before the relay do read harness files. They stay, because what they capture is exported nowhere else:

| Reader | File | What it captures | Why no OTLP substitute |
|---|---|---|---|
| `readRolloutReplies` (internal/agents/codex) | the Codex rollout | the assistant's reply text (`terma.assistant.message`) | Codex exports what was asked, never what it said |
| `readThreadTitle` (internal/agents/codex) | `$CODEX_HOME/session_index.jsonl` | the thread's name (`terma.session.title`) | the title conversation exports its spans, not the name it chose |
| Codex funding capture | the Codex rollout | rate-limit and quota snapshots (`terma.session.quota`) | not in Codex's OTLP |
| `readDesktopActivity` (internal/agents/codex) | the Codex rollout | Desktop turn summaries, compactions, approvals | not in Codex's OTLP |
| `rolloutSpawn` (internal/agents/codex) | a subagent's rollout, first line | the subagent's parent link | not in Codex's OTLP |
| `readFunding` (internal/agents/claude) | `~/.claude.json` | account state and credential-presence hints (`terma.session.account`) | not in Claude's OTLP |

Each read is bounded and confined as `CLAUDE.md` describes, and replies and titles travel under the prompt-consent gate. These should be replaced the day a harness exports the same facts. Nothing new may add such a read.

## Findings

1. **Session keys:**
   - **Claude** stamps `session.id` on every log, span and metric data point.
   - **OpenCode's plugin** stamps `session.id` on its logs and spans.
   - **Codex** stamps `conversation.id` on its logs and on no metric. It names the conversation on its turn span only, as `thread.id`; elsewhere `thread.id` is an OS thread number.
2. **Codex's spans need the trace join.** A turn's child spans are exported before the turn span that names the session, as each ends. The relay learns their trace from Codex's own mid-turn logs, which carry both the conversation and the trace id. That cut unattributable spans from about 310 to about 140 per run and raised forwarded records from about 456 to about 628. A turn longer than the ordinary hold keeps its children.
3. **Codex's metrics name no session, and are attributed by process once it exits.** A `codex exec` that named one claimed session has its metrics, process-level spans and nameless log events go to that project about 10 seconds after it exits, marked `terma.relay.attribution=process`. The live contract runs the same metric checks through the relay as without it. A process that named more than one session — a TUI that ran its title conversation, the shared daemon — has them dropped. (Until a review found it could leak, this attributed by process while the process ran.)
3b. **Codex's TUI titles each thread in a second conversation** of its own: its own id, its own model call and cost, no hook, started about 7 s after the first reply (0.158, live). Nothing links it to the thread, and its signature is one a developer can produce, so it is not collected; the title itself is read from Codex's session index.
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
| Codex metrics and process-level spans | **Lost:** about 470 records a run. Now attributed once their process exits having named one claimed session; dropped for a process that served several. |
| Codex TUI title conversation | Not collected: unclaimed, and nothing proves it is Codex's own. (Adopting it by signature could collect a personal conversation; removed after review.) |
| A shared process's unnamed trace (review) | **Leaked:** sent with the one claimed session the process had shown, before a personal thread named the trace. Now waits for the trace to be named. |
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

- **A squatter on the relay's port** receives what the agents send. This includes another local user on a shared machine, since loopback is shared. The fix is TLS to the relay with a pinned certificate. Claude's exporter takes a CA file; Codex's and the plugin's haven't been checked.
- **An unresolved sender** falls back to the session, counted as `sender_unresolved`. That happens for an agent running as another user, or on a platform without `/proc` or `proc_info`.
- **The first Codex session after an idle-out** loses `conversation_starts` (finding 6) unless the relay runs as a service. `TestRelayDaemon` shows that a service relay killed with `SIGKILL` is back without any hook, and that a Codex session delivers its whole contract, `conversation_starts` included.
- **Upstream acceptance hasn't been tested against the real Terma ingest.** The payload is the agents' own native OTLP with one resource attribute added, but no live key was used.

## Recommendation

**Go.** On every build in the matrix, across three harnesses, the relay separated opted-in work from everything else, found the project from a claim, and withheld content per project. Every break attempt that got through has a fix and a test. What it gates on is small:
- a session id the harness already stamps;
- a trace join that Codex's span shape makes necessary;
- a process check against what the claiming hook saw.

The server-side correlation stays on the server.
