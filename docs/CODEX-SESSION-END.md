# Codex 0.158.0 SessionEnd investigation

The missing session-end event originates in Codex's shutdown path, before
Terma's handler receives the hook. It reproduces with synchronous hooks whose
entire command is a shell builtin writing a marker file. SessionStart writes
its marker; Codex completes a fixture-backed turn and exits with status zero;
SessionEnd sometimes leaves no marker.

## Evidence

Tests used the installed Codex 0.158.0 on macOS, a fresh trusted repository and
isolated CODEX_HOME for each run, dummy API credentials, and a loopback Responses
provider. Hook trust is explicitly bypassed by Codex's automation flag. No live
provider or Terma backend is involved.

The normal telemetry test delivered all expected native logs, spans and metric
values in a failing run. Its hook spans showed SessionStart, UserPromptSubmit,
PreToolUse, PostToolUse and Stop completing. Stop took about 28 ms. No
SessionEnd command span or captured payload arrived, and no end event was queued.

The independent `TestCodexSessionEndProbe` replaces the repository hooks with:

```json
{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "printf started > .codex/probe-start", "timeout": 3}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "printf ended > .codex/probe-end", "timeout": 3}]}]
  }
}
```

That probe reproduced the omission in 3 of 10 runs. It has no Terma handler, payload
recording shim, spool or network operation in either hook. It checks the markers
after the Codex process has exited; these hooks are synchronous.

Native debug logs from 2026-09-28 show the timing dependence:

| Run | Clearing app-server listeners | Core receives `Op::Shutdown` | Gap | End hook |
| --- | --- | --- | --- | --- |
| Telemetry | 13:54:56.711751Z | 13:55:04.067083Z | 7.36 s | Missing |
| Telemetry | 13:55:56.863906Z | Before 13:56:00.131419Z (shutdown completed) | At most 3.27 s | Delivered |
| Marker probe | 13:59:23.487264Z | 13:59:29.924391Z | 6.44 s | Missing |

Failed runs end with a dropped-event message during core shutdown and lack the
`Shutting down Codex instance` log that follows completion of SessionEnd hooks.
The marker probe establishes the upstream omission; the source and timing below
identify the cleanup deadline as the likely cause. We have not rebuilt Codex
with a patched deadline to prove the causal fix.

## Shutdown ordering and conflicting deadlines

These links are pinned to the tested release:

1. [`codex exec`](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/exec/src/lib.rs#L1296)
   requests `thread/unsubscribe`, then immediately shuts down the in-process
   client. The unsubscribe response acknowledges subscription removal; it does
   not wait for the session-end hook.
2. The [in-process runtime](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/app-server/src/in_process.rs#L793)
   gives its processor **five seconds** to finish, then aborts it. This timeout
   branch does not report a failure to `codex exec`.
3. The [processor's cleanup sequence](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/app-server/src/in_process.rs#L591)
   clears listeners, drains background tasks, then shuts down threads.
   [Background draining and thread shutdown](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/app-server/src/request_processors/thread_processor.rs#L1267)
   each allow **ten seconds**, exceeding the outer five-second budget.
4. [Core session teardown](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/core/src/session/handlers.rs#L287)
   performs several cleanup operations before calling `run_session_end_hooks`.
   The hook itself is [capped at three seconds](https://github.com/openai/codex/blob/rust-v0.158.0/codex-rs/hooks/src/events/session_end.rs#L20)
   to fit the outer budget, but that does not reserve time for earlier cleanup.

This permits process exit to race session teardown. Increasing the receiver's
wait cannot recover a hook after the harness has exited. Increasing Terma's
hook timeout also cannot repair the outer deadline and would exceed Codex's cap.

An upstream fix should reserve and await a bounded session-end phase before
aborting the runtime, with consistent nested deadlines and visible diagnostics
on timeout. Merely raising five seconds would reduce the race without making
the shutdown contract explicit. A regression should delay earlier cleanup and
assert that SessionEnd still runs or shutdown reports failure.

## Reproduce

Build `test/live/bin/terma` from the repository root, then run:

```sh
cd live
TERMA_ENV=dev TERMA_LIVE=1 TERMA_LIVE_BINARY="$PWD/bin/terma" \
  TERMA_LIVE_CODEX_VERSIONS=installed \
  TERMA_LIVE_CODEX_LIFECYCLE_PROBE=1 \
  TERMA_LIVE_CODEX_RUST_LOG='codex_core=debug,codex_app_server=debug,codex_hooks=trace' \
  go test -v -count=10 -timeout=5m -run '^TestCodexSessionEndProbe$' .
```

The probe is opt-in because it deliberately substitutes marker hooks for Terma
and diagnoses a known upstream race. Ordinary telemetry contracts retain their
strict session-end assertion, except where `TERMA_LIVE_KNOWN_UPSTREAM` names
`codex-session-end`: pull-request CI sets it and logs `KNOWN UPSTREAM` for a run
the race hit; local runs and the nightly live workflow do not. `TERMA_LIVE_CODEX_RUST_LOG` also works with those
contracts; failures print native stdout/stderr and exported lifecycle spans.
