# Local relay spike

## Why

A repository cannot switch an agent's telemetry on or off for itself:
- **Claude Code and Codex** refuse OTel settings from repository config.
- **The desktop apps** never pass through terma's PATH shims.

The only exporter config that reaches every surface is global. Global config, though, sends everything: personal use and unrelated repositories included, under one key, with nothing that says which project a record belongs to.

The spike tests a way around that. Every agent's global exporter sends to a relay terma runs on loopback. The relay forwards a record only if a hook in an opted-in repository claimed its session, and sends it to that repository's project with that project's key. Anything unclaimed never leaves the machine and never touches the disk.

## How it works

1. **`terma relay setup`** (hidden) points Claude's and Codex's global exporters at `http://127.0.0.1:43180`. It writes a random local token into their exporter headers; the relay refuses any request without it. Content is left on in the exporters, because the relay applies each project's content policy itself.
2. **Hooks claim sessions.** Any hook in a repository with a binding writes `~/.config/terma/relay/claims/<session>.json`, holding `{project_id, tool, repo}`. Claims come from two places:
   - `hookrun.Env.emitFor`, which every spooled event passes through;
   - `hookrun.ClaimFromPayload`, for hooks that spool nothing, such as a Codex tool call that edits no file. It reads the session from the hook's payload.

   A claim is rewritten at most every 5 minutes and expires after 4 hours. Hooks write claims only on a machine that ran `terma relay setup`.
3. **The hook that claims also starts the relay** (`terma relay run --quiet`, detached) if its lock is free. The relay exits after 30 idle minutes.
4. **The relay splits every export by session:**
   - by record for logs, by span for traces, by data point for metrics;
   - using `session.id` (Claude), `conversation.id` (Codex logs), and `thread.id` on Codex's `session_task.turn` span only;
   - a span that names no session belongs to its trace's session. A span whose trace hasn't been named yet is held under the trace.
5. **Each session's part is then handled one of three ways:**
   - **Claimed, and this machine holds a key for the project:** the content policy is applied, `mirador.project.id` is stamped on the resource, and the part is POSTed in native OTLP (protobuf) to the project's own ingest host (`projectEndpoint`). Retries are per project.
   - **Claimed, but no key:** dropped. The developer never opted in to that project on this machine.
   - **Unclaimed:** held in memory for 2 minutes (`TERMA_RELAY_HOLD` overrides this), then dropped.
6. **Content policy:** the project's routing record decides. With no record, both prompts and tool content are withheld.
   - **Prompts off:** `prompt`, `response` and `user_prompt` are blanked with each harness's own marker (`<REDACTED>` for Claude, `[REDACTED]` for Codex).
   - **Tool content off:** `tool_parameters`, `tool_input`, `full_command`, `bash_command`, `arguments` and `output` are removed, and so are the `tool.output` and `tool.input` span events.
7. **`terma relay status`** prints the counters, keyed by reason (`received`, `forwarded`, `dropped.<reason>`, `released_after_hold`). They are also written to `relay/stats.json` on exit.

## What the live runs showed

Every scenario passed on every build in the matrix (2026-09-29).

Every scenario ran against real harness builds with deterministic fake model providers (`live/relay_test.go`): Claude Code 2.1.282–2.1.284 and Codex 0.157.1–0.159.0.

| Scenario | Result |
|---|---|
| Opted-in Claude session, content allowed / withheld | The full direct-export contract arrives: logs, traces, metrics, session and tool joins, and the project key on every request. Only the claimed session's records arrive, and every resource carries the project. Withheld content never leaves. |
| Opted-in Codex session, content allowed / withheld | The same, minus metrics (see below). |
| Session outside any repository | The relay received 22 records and forwarded 0 (`dropped.unclaimed_expired`). |
| Session in a repository with no binding | Received 22, forwarded 0 (`unclaimed_expired`). |
| Installed repository, no key on this machine | Received 33, forwarded 0 (`dropped.no_key`). |
| Cold start (relay not running when the agent starts) | The first hook started it and the whole contract arrived. |
| Claim written seconds after the session's exports | All 22 held records were released and forwarded (`released_after_hold`). |

## Findings

1. **Session keys.**
   - **Claude** stamps `session.id` on every log, span and metric data point.
   - **Codex** stamps `conversation.id` on every log, and on no metric.
   - **Codex spans** carry the conversation id only on `session_task.turn`, as `thread.id`. On every other Codex span, `thread.id` is the tracing library's OS thread number (with `thread.name=tokio-rt-worker`). Treating it as a session key mis-attributes spans, so a numeric `thread.id` is ignored and those spans go by their trace.
2. **Codex's spans need the trace join, and the hold.** Per run the relay received about 935 Codex records:
   - About 457 were forwarded.
   - About 250 of those came out of the hold (`released_after_hold`). Codex exports a turn's child spans before the `session_task.turn` span that names the session, so the children wait under their trace until it arrives.
   - About 310 spans sit in traces no span ever names: hook runtime, rollout persistence and other process-level work. They are dropped as `no_session_trace`. None of them is needed for the contract.
3. **All Codex metrics are dropped** (`no_session_id`, about 167 points per run). Every number in them can be rebuilt per session from Codex's log events (`codex.sse_event` / `response.completed`, `codex.api_request`, `codex.tool_result`). A backend reading Codex usage from metrics would have to read the logs instead.
4. **Claude records tool content in a span event** (`claude_code.tool` → `tool.output` event with `bash_command`) as well as in attributes. The golden attribute lists don't see span events. The relay drops those events when tool content is withheld, and the live test's `leakedFields` names any content that reaches upstream.
5. **terma's own Codex reply capture** (`terma.assistant.message`) used to read the machine-wide Codex config as consent. Under the relay, that config always allows prompts, because the relay decides. So with the relay set up, only the project's routing record can consent (`codexRepliesConsented`).
6. **Cold start:** a headless run's only export is the flush at exit, well after the `SessionStart` hook has started the relay. Long interactive sessions export every few seconds, and the 2-minute hold covers a first batch that arrives before the claim does. No session in any run lost a record waiting for its claim.
7. **Hook budget:** `prepare-commit-msg` stayed at a median of 21 ms, the same as before the change (the budget is 50 ms), with the OTLP protobuf types linked into the binary. The collector packages are avoided: they would bring gRPC into every hook.

## Not done in the spike

- `terma status` and `terma doctor` don't know the relay endpoint. They compare exporter endpoints with the profile's OTLP URL, so a relayed agent reads as "not exporting to Terma".
- **Nothing is persisted.** An upstream failure is retried in memory, and whatever is still queued at exit is counted as lost (`upstream_lost_at_exit`), not spooled.
- **Not covered:**
  - OpenCode: its plugin routes itself;
  - Cursor and Antigravity: they're hooks-only;
  - the desktop apps: not driven by `live/` yet.
- `OTEL_METRICS_INCLUDE_SESSION_ID` is not pinned in the global Claude config, so a developer who sets it to `false` makes Claude's metrics unattributable.

## Recommendation

**Go.** The relay separates opted-in work from everything else on real Claude and Codex builds, and a claim is enough to find the project. It doesn't reimplement the server's correlation: it's a gate, with one join (trace → session) that Codex's span shape makes necessary.

The server-side variant (the same claim check at the edge, with no local process) would need three things:
- every signal to carry a session key, which Codex metrics don't;
- a claim tied to the developer, not only to a project key;
- a hold that discards unclaimed data before anything persists.

## Running it

```bash
cd live && make telemetry          # includes TestRelay*, no credentials
TERMA_LIVE_CLAUDE_VERSIONS=last3 TERMA_LIVE_CODEX_VERSIONS=last3 make live RUN='TestRelay.*'
```

The nightly `live.yml` runs them on the last three releases of each harness. A release that stops stamping a session key on a surface, or that starts exporting content somewhere new, fails there. Relay goldens live in `live/golden/relay/` (`LIVE_UPDATE_GOLDEN=1` rewrites them from the newest build).
