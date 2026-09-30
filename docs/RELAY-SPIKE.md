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

**Versions:** the matrix ran every scenario above, plus the workloads and the direct telemetry contracts, from frozen binaries (578 runs):
- **Claude Code:** 2.1.280–2.1.285.
- **Codex:** 0.156.0–0.159.0, with 0.159.1 in part as it came out during the run.
- **OpenCode:** 1.18.30–1.18.33.

Every relay scenario passed on every build. Two failures came from elsewhere:
- **`TestRelayWorkloadsCodex`:** on some runs the relayed `codex exec` exited without its shutdown. That is Codex's known exit without `SessionEnd` (`docs/CODEX-SESSION-END.md`), and with it the shutdown's own telemetry never left the process (`session_loop`, `op.dispatch.shutdown`, the last metrics flush). It is not the relay: across five runs each, direct exec shut down cleanly 2 times and relayed exec 3 times. The comparison now notes what an unshut run never sent. It still requires that the relay dropped nothing and forwarded everything it received.
- **`TestClaudeTelemetry/redacted`:** fails on 2.1.280 and 2.1.281 with a terma built from `main` too. Claude exported the prompt with prompts off; 2.1.282 and later do not. The relay's own withheld-content scenario passed on those builds, because the relay removes content itself, whatever the harness's switches say.

Later additions, run on the installed builds and the last four Codex releases:
- Claude Desktop, on its pinned 2.1.202 and on the newest build;
- Codex's app-server and daemon, on 0.157.0–0.159.1;
- Pi 0.99.1.

### Workload equivalence

`live/relay_workloads_test.go` runs every workload twice against identical fake providers, once exporting directly and once through the relay. It then requires the same telemetry upstream (every log event as many times, every span and metric name) and zero drops.

| Harness | Workloads |
|---|---|
| Claude | reply, Bash, a failing Bash, 400 KB of tool output, 8 sequential tools, 3 parallel tools in one response, Write/Read/Edit, a subagent, Unicode and RTL, a provider overload (529) retried |
| Codex | reply, shell, a failing shell, 400 KB of output, 6 tools, a file written by the shell |
| OpenCode | reply, a bash tool call |
| Pi | reply, a bash tool call, a file written (plus: Pi outside any repository reaches nothing upstream) |
| omp | reply, a bash tool call (plus: omp outside any repository exports nothing at all) |

All 18 pass on the installed builds (Claude 2.1.284, Codex 0.158.0, OpenCode 1.18.33). In every relayed run the relay forwarded everything it received. The only difference found is Claude's `retention_sweep`, a housekeeping event on Claude's own schedule that a run may or may not emit.

The Codex TUI (`TestRelayCodexTUITitle`) forwarded 1,614 of 1,614 records, its title conversation included.

### omp

omp (oh-my-pi, PR #21, merged into this branch) has a native OTLP exporter configured only by `OTEL_*` variables, which it reads once at startup, before any hook or extension loads. So only a wrapper could configure it, and that was the first route here: a PATH shim handing omp the relay's variables. That route is gone, for two reasons:
- **Shims are being removed.**
- **Tools inherited it:** everything omp ran inherited the variables, the relay's token included.

A committed hook that sets the variables at load exports nothing (verified on 18.3).

omp is a Pi fork with the same extension events, so it runs terma's Pi-family extension (`internal/harness/pi/terma.ts`). `relay setup --harness omp` writes it to `~/.omp/agent/extensions/terma-relay.ts` with `agent: "omp"`:
- **What it exports:** it exports from omp's own events (usage and cost from `message_end`, tool calls, `omp.user_prompt`) and sets no environment.
- **Claims:** `lifecycle` is false, so it only claims the session at each prompt (`omp-prompt`). omp's committed hook file already reports session start, end and file edits, under omp's own session id (`ctx.sessionManager.getSessionId()`).
- **Load locations:** user extensions load from `~/.omp/agent/extensions/` (a file, or a directory's `index.ts`), from `hooks/pre/`, and from `config.yml`'s `extensions:` list. All three were verified on 18.3.

Tests on 18.3: reply and bash workloads direct vs relay; outside a bound repository nothing reaches upstream; and omp's tools inherit no `OTEL_*` (`TestRelayOmpToolsGetNoExporter`).

### Codex's app-server (Desktop, the daemon)

Codex Desktop, the IDE extension and, since 0.157, the interactive TUI run their threads in `codex app-server`. A bare `codex` with a daemon running attaches to it (`daemon_auto_start`, stable and on). Any `-c` override or `--dangerously-bypass-hook-trust` runs the TUI in-process instead; so do `codex exec` and terma's own Codex shim.

The daemon is one process per `CODEX_HOME`, reached at `$CODEX_HOME/app-server-control/app-server-control.sock`. It serves every workspace, and it spawns every thread's hooks and exports all their telemetry (`service.name=codex-app-server`). The TUI process exports only a few metrics of its own, which name no session. What that means for the relay:

- **Process scoping separates nothing within the daemon:** every claim names the daemon, so only `conversation.id` tells threads apart. A loaded thread keeps its own working directory: `thread/resume` with another `cwd` is ignored, and the resumed turn runs in the repository with its hooks. A daemon or Desktop restart unloads every thread. A thread resumed from a personal directory after that runs in a new process, and process scoping holds it back (`TestRelayCodexDesktopResumedElsewhere`: 412 records dropped as `uncovered_process`).
- **Originator names the first client, not the thread's:** it is global to the daemon and set by the first client's `clientInfo.name` (a TUI connecting after Desktop reports `Codex_Desktop`). The relay keeps every client a process served and adopts only when all of them are single-workspace. What actually prevents a wrong adoption is evidence: a process with any unclaimed thread the developer started is ambiguous. The relay learns every part of an export before it routes any, so a personal thread's records and its title's in the same batch make the process ambiguous first. The daemon test covers this: under sabotage the personal title was adopted and the test failed.
- **The thread's own work is named by `session_loop`:** its internal spans (turn context, rollout persistence, its hook commands, shutdown) sit under the `session_loop` root span, which names the thread as `thread_id` (underscore). That is now a session key, numeric values excluded. `session_loop` is exported when the loop ends, so these spans wait in the trace hold until then.
- **A thread's start waits for its first turn:** app-server exports `conversation_starts` at `thread/start`, and the first hook fires with the first turn, whenever the developer types. An unclaimed conversation start now waits as long as a trace (30 minutes), not 2 minutes. `TestRelayCodexDesktop` waits past the test's hold before the first turn, and the start still arrives.
- **Metrics:** the app-server exported no OTLP metrics in any run, 0.157.0 through 0.159.1, including after a clean SIGTERM. The only session-less counters seen come from the TUI client processes, and those name no session and are dropped.
- **The exporter is read once:** the daemon reads `[otel]` when it starts. A change reaches it only after `codex app-server daemon restart`, and Codex warns that running work may be interrupted. `relay setup` says so when a daemon predates it, and doctor warns until the daemon has restarted (`harness.RunningCodexDaemon`, read from `$CODEX_HOME/app-server-daemon/daemon.pid`). terma never restarts it.

Tests (`live/codex_appserver.go` drives app-server over stdio JSON-RPC the way Desktop does, and a sandbox daemon with a TUI attached to it):
- **`TestRelayCodexDesktop`:** one process holds a repository thread and a personal thread. The first reaches its project, the second nothing.
- **`TestRelayCodexDesktopResumedElsewhere`:** a thread is resumed from a personal directory after a restart, and nothing of the resumed turn leaves.
- **`TestRelayCodexDaemonTUI`:** a TUI attached to the daemon, verified by the claim naming the daemon's pid. The repository thread and its adopted title reach the project. A personal thread in the same daemon, and its title, reach nothing.

All three pass on 0.157.0 through 0.159.1. The daemon test needs a short `CODEX_HOME`, because a socket path is limited to 104 bytes.

### Claude Desktop

Claude Desktop's Code tab never runs the `claude` on PATH, so terma's per-repository shim never reaches it. What reaches it is the user's `settings.json`, which is where `relay setup` points the exporter. Read from the app bundle (Desktop 1.19367.0):
- **Binary:** its own pinned Claude Code (2.1.202), under `~/Library/Application Support/Claude/claude-code/<version>/`.
- **Launch:** through the Agent SDK, under the `Contents/Helpers/disclaimer` helper, which stays alive as the parent (Claude → disclaimer → claude).
- **Transport:** `stream-json` over pipes, with no terminal, so the status line never renders.
- **Settings:** `--setting-sources=user,project,local`, so the user's exporter settings and the repository's hooks both apply.
- **Service name:** `service.name=claude-code-desktop`, set through `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`. The relay routes by session, never by service name. The backend's adapter must accept the Desktop name.
- **Worktree mode:** "use worktree" creates linked worktrees at `<repo>/.claude/worktrees/<name>`, which find the main checkout's binding.

`TestRelayClaudeDesktop` (`live/claude_desktop.go`) reproduces that launch on the pinned build and on the newest. On one relay it runs a repository session, a worktree session and a personal session. The first two reach the project with its key, and their hooks ran. The personal session was received and dropped. It passes on 2.1.202 and 2.1.284.

**Not covered:** Desktop's cowork mode (`claude-code-vm`) runs Claude Code in a Linux VM:
- with user settings only (so no repository hooks, and no claims);
- with its own `CLAUDE_CONFIG_DIR`;
- behind a userspace network where 127.0.0.1 is not the host.

Nothing it does reaches the relay, so it is lost rather than leaked.

### Pi

Pi (`@earendil-works/pi-coding-agent`) has no OpenTelemetry, so terma's extension (`internal/harness/pi/terma.ts`) is its exporter. `terma relay setup --harness pi` writes it into Pi's agent directory (`PI_CODING_AGENT_DIR`, else `~/.pi/agent`) pointed at the relay. It emits OTLP/JSON under the GenAI conventions:
- a `chat <model>` span per model response, with usage and cost;
- an `execute_tool <tool>` span per tool call;
- a `pi.user_prompt` log per prompt.

Every record names Pi's own session id as `session.id`. The extension also calls `terma hook`:
- `pi-session-start` at session start;
- `pi-prompt` on every prompt, which claims the session without announcing it again, as Claude's `user-prompt-submit` does;
- `pi-file-edit` for `write` and `edit`;
- `pi-session-end`.

The relay withholds the prompt body, `gen_ai.completion` and the tool arguments and results when content is off.

Tested on the installed 0.99.1 (`live/relay_pi_test.go`):
- **Workload equivalence:** reply, bash, and write, each direct vs relay, with zero drops.
- **Content:** allowed and withheld. The withheld case was sabotaged: without `pi.user_prompt` in the gate, the prompt body leaked, and the test caught it.
- **Negative control:** Pi outside any bound repository sends to the relay, and nothing reaches upstream.
- **Attribution:** the commit of a file Pi wrote carries `Agent-Tool: pi` and Pi's session.

Pi is not yet selectable in `terma setup` ("Coming Soon"). The relay is its only route.

### Hermes

Hermes (Nous Research, 0.20.4) has no usable OTLP export. Its NeMo Relay exporter names no session, drops usage on streamed calls and carries the whole system prompt. Its shell hooks are user-level only and do not fire in its TUI, which is the default front end. Its Python plugins run in every front end, so terma's plugin (`internal/harness/hermes`) is its exporter:
- **Installation:** `terma relay setup --harness hermes` writes the plugin into `$HERMES_HOME/plugins/terma/` and enables it with `hermes plugins enable terma` (plugins are opt-in).
- **Spans:** a `chat <model>` span per provider call, with `gen_ai.usage.*` and Hermes's own cost estimate (`agent.usage_pricing`, as its Langfuse plugin uses; none on a subscription-included route), and an `execute_tool <tool>` span per tool call.
- **Logs:** a `hermes.user_prompt` and a `hermes.assistant_response` log per turn, whose bodies the relay withholds with content.
- **Hooks:** it calls `terma hook hermes-*` by terma's absolute path. `hermes-prompt` claims without announcing, and `write_file` and `patch` report their file. The workspace comes from Hermes's `resolve_agent_cwd()`; in the TUI the process's own cwd is where its backend started.

It passes on 0.20.4:
- **Workloads:** a reply and a file write, each direct vs relay.
- **Content:** allowed and withheld. The withheld case was sabotaged: without the two log events in the gate, both bodies leaked, and the test caught it.
- **Negative control:** a session outside any repository reaches nothing upstream.
- **Attribution:** the commit is stamped `Agent-Tool: hermes`.

**Not seen:** auxiliary calls fire no plugin hook (the session-title request; compression, likely), so their spend is not seen.

### Gemini CLI

Gemini CLI (0.62) exports OTLP natively, configured entirely from the user settings file (`~/.gemini/settings.json` `telemetry`). It records `session.id` on every log record and metric point, and `gen_ai.conversation.id` on spans.

The file has no headers setting, and headers could only come from `OTEL_EXPORTER_OTLP_HEADERS`, which Gemini's tools inherit. So the relay also accepts its token as the endpoint path's first segment (`http://127.0.0.1:43180/<token>`; Gemini appends `/v1/<signal>`). The match is exact and constant-time. Nothing is set in any environment, and `TestRelayGeminiToolsGetNoExporter` checks it.

Claims come from terma's user-level Gemini extension (`~/.gemini/extensions/terma`):
- **Where it fires:** its hooks fire in every folder with no enable step, as children of the exporting process.
- **Its events:** `SessionStart`, `BeforeAgent` (claim only), `AfterTool` (file edits from `write_file` and `replace`) and `SessionEnd`, calling `terma hook gemini-*` by terma's absolute path.

`relay setup --harness gemini` changes only the settings file's `telemetry` block, and refuses a file it cannot parse.

With prompts off, Gemini still sends two kinds of content:
- the system prompt and tool definitions (`gen_ai.system_instructions`, `gen_ai.tool.definitions`);
- the `-p` prompt in the resource's `process.command_args`.

The relay drops them. It now filters resource attributes too (`resourcePromptFields`), and the live leak check now looks at resources and metric points. With the resource filter sabotaged, the withheld run leaked `process.command_args` from spans, logs and metrics, and the test caught it.

Passes on 0.62.0:
- **Workloads:** a reply, `write_file` and `run_shell_command`, each direct vs relay.
- **Content:** allowed and withheld.
- **Negative control:** a session outside any repository.
- **Tools:** they inherit nothing.
- **Attribution:** the commit is stamped `Agent-Tool: gemini`.

**Not seen:** Gemini exports no cost.

### DeepSeek Harness

DeepSeek Harness (`dsh`, `@deepseek-ai/dsh`, 0.2.0-rc.2) sends its own OTLP to DeepSeek's collector, without usage. Its Cordis plugins load from the user's home layer, `$DSH_HOME/cordis.patch.yml`, which every profile loads. terma's plugin (`internal/harness/dsh/terma.mjs`) is its exporter:
- **Installation:** `relay setup --harness dsh` writes it to `$DSH_HOME/plugins/terma-relay.mjs` and appends one insert entry to the patch file, keeping every other byte, and only if the entry is missing.
- **Spans:** a `chat` span per model response, with usage from the `assistant/message` session event, and one per auxiliary call too, from the `llm/stream` waterfall (the session title, with its `dsh.purpose`). Plus an `execute_tool` span per tool call.
- **Logs:** a `dsh.user_prompt` and a `dsh.assistant_response` log per turn.
- **Hooks:** it calls `terma hook dsh-*` (session start, a claim-only prompt, `write` and `edit` files, session end at exit) through the same handlers as Pi and Hermes.

It sets no environment, because dsh passes its own to every tool.

Passes on 0.2.0-rc.2:
- **Workloads:** reply, write and bash, each direct vs relay.
- **Content:** allowed and withheld. The withheld case was sabotaged, both log bodies leaked, and the test caught it.
- **Negative control:** a session outside any repository.
- **Attribution:** the commit is stamped `Agent-Tool: dsh`.
- **Auxiliary calls:** the title call is spanned, which Hermes cannot show.

**Not seen:** dsh computes no cost.

### Goose, Aider

- **Goose (1.52):** native OTLP, but the config file holds only the endpoint, and Goose copies it into its own environment (`set_var`), so every tool it runs inherits it, token included. It has hooks but no in-process plugin API, so terma cannot give it an exporter of its own. **Not supported** until upstream stops exporting the endpoint to tools.
- **Aider (0.86):** no OpenTelemetry, no plugin or hook API, no session id. The only in-process route is a `.pth` file that monkeypatches aider internals, and a virtualenv rebuild loses it. **Not supported.** Aider's own auto-commits do run terma's commit hooks.

### Cursor

cursor-agent (2026.09.08) bundles an OTLP exporter, but its tracer is fixed to Cursor's own backend (`${backendUrl}/v1/traces`, Cursor's token, `service.name=cursor-agent-cli`). Nothing `relay setup` writes redirects it, so Cursor never reaches the relay and is seen through its hooks alone.

`TestRelayCursorHooks` covers Cursor on a relay machine. Its hooks deliver through the spool as before, stay silent, and claim the conversation under `cursor`, harmlessly, since nothing exports under it. The relay receives nothing. Cursor itself needs a `CURSOR_API_KEY`, so it runs in the credentialed suite only.

### T3 Code

T3 Code (0.0.42) is a local server with a web front end, and runs other agents:
- **Codex:** one `codex app-server` per T3 thread, in the thread's workspace.
- **Claude Code:** through the Agent SDK, with setting sources user, project and local.
- **Environment:** it sets no `OTEL_*` of its own and runs no collector.

So the agents' user-level exporters and the repository's hooks work as they do anywhere, and each agent exports from the process that runs its hooks. `TestRelayT3` drives T3's orchestration API with a Codex and a Claude thread in the bound repository and one of each in a personal project.

It passes on 0.0.42: the repository's Codex and Claude threads reach the project, prompts included, and the personal ones reach nothing. T3 keeps its agents running, and their exporters send on their own schedule (Claude's logs every 5 s), so the test waits for the repository's records before stopping T3.

**Not caught:** T3's thread titles run `claude -p` in a temporary directory, so no hook claims them and they are not forwarded.

### Tools must not inherit the relay

An agent's tools must never inherit the relay's `OTEL_*` variables, since `OTEL_EXPORTER_OTLP_HEADERS` carries its token. If they did:
- an OTel-instrumented program under test would export to the relay (and be dropped), not to its own collector;
- cursor-agent merges `OTEL_EXPORTER_OTLP_HEADERS` into what it sends Cursor's backend.

Where each agent stands:
- **Claude Code:** strips them from its tools (`TestRelayClaudeToolsGetNoExporter`, on 2.1.202 and 2.1.284).
- **omp:** its first route, through a shim, handed the variables to omp, and omp's tools inherited them. The extension route sets none, and `TestRelayOmpToolsGetNoExporter` checks it.

### Harness summary

| Harness | Exports OTLP? | Claims possible? | Status |
|---|---|---|---|
| T3 Code | its agents': a `codex app-server` per thread, Claude's Agent SDK | through those agents' hooks | works through the relay unchanged (`TestRelayT3`) |
| Gemini CLI | natively, from its settings file; the token in the endpoint's path | terma's user-level Gemini extension | done |
| DeepSeek Harness | through terma's Cordis plugin | the plugin calls `terma hook dsh-*` | done |
| Goose | natively, but its tools inherit the endpoint | hooks | not supported (tools would inherit the token) |
| Aider | no | no | not supported |
| Hermes | through terma's plugin | the plugin calls `terma hook hermes-*` | done |
| Pi | through terma's extension | the extension calls `terma hook pi-*` | done |
| Cursor | no (its own backend only) | hooks | unaffected; never reaches the relay |
| omp | through terma's extension (its native exporter is environment-only) | committed hook file, plus the extension's `omp-prompt` | done, no shim |

## What still reads files on disk

The relay's own attribution reads no harness files: everything it knows comes from OTLP and hook payloads. Some hook features from before the relay do read harness files. They stay, because what they capture is exported nowhere else:

| Reader | File | What it captures | Why no OTLP substitute |
|---|---|---|---|
| `harness.ReadCodexReplies` | the Codex rollout | the assistant's reply text (`terma.assistant.message`) | Codex exports what was asked, never what it said |
| `harness.ReadCodexThreadTitle` | `$CODEX_HOME/session_index.jsonl` | the thread's name (`terma.session.title`) | the title conversation exports its spans, not the name it chose |
| Codex funding capture | the Codex rollout | rate-limit and quota snapshots (`terma.session.quota`) | not in Codex's OTLP |
| `harness.ReadCodexDesktopActivity` | the Codex rollout | Desktop turn summaries, compactions, approvals | not in Codex's OTLP |
| `harness.CodexRolloutSpawn` | a subagent's rollout, first line | the subagent's parent link | not in Codex's OTLP |
| `harness.ClaudeFunding` | `~/.claude.json` | account state and credential-presence hints (`terma.session.account`) | not in Claude's OTLP |

Each read is bounded and confined as `CLAUDE.md` describes, and replies and titles travel under the prompt-consent gate. These should be replaced the day a harness exports the same facts. Nothing new may add such a read.

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
