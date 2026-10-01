# Harness telemetry contracts

The live suite tests real Claude Code and Codex binaries against a local OTLP
receiver. `TestClaudeTelemetry` and `TestCodexTelemetry` use deterministic local
provider responses: one shell call followed by one text reply, with known token
counts. The harness runs the tool and emits the telemetry itself. No telemetry
is injected into the receiver by these scenarios, and no provider credentials or
paid model calls are needed.

```sh
cd live
TERMA_ENV=dev make telemetry
# Test only the installed versions rather than downloading other releases:
TERMA_ENV=dev TERMA_LIVE_CLAUDE_VERSIONS=installed \
  TERMA_LIVE_CODEX_VERSIONS=installed make telemetry
```

PR CI runs these scenarios with Claude 2.1.283 and Codex 0.158.0. The nightly live
workflow also runs them against its latest-three-release matrix. Authenticated
API and subscription scenarios remain separate checks; a controlled provider
response does not establish provider compatibility, account identity or quota
coverage.

## Assertions

| Surface | Required evidence |
| --- | --- |
| Transport | Decodable OTLP and the correct project key on **every** logs, traces and metrics export |
| Receiver | Complete decoded export envelopes, including resource/scope metadata, typed values, log correlation IDs, span timing/status/events/links, and metric points, dimensions, temporality and buckets; protobuf and JSON decoding are regression-tested |
| Prompts and replies | Exact prompt and reply content when enabled; lengths and message IDs; Claude native replies and Codex replies delivered by Terma's rollout hook |
| Model requests | Both requests in the tool/reply turn; model, session, request identity where supplied, usage, cache counts, timing and status; Claude cost and time to first token; Codex reasoning counts |
| Tool activity | Permission decision, decision source, tool name and call ID, matching result, success, duration, arguments and output; Claude output is a trace event |
| Traces | Required turn/request/tool spans, valid trace and span IDs, parent joins, timestamps and status; Claude log-to-span joins; Codex turn and per-response usage |
| Metrics | Actual point values and dimensions, not just names; exact token/cache/reasoning totals, Claude session count and cost agreement with request logs, Codex request/tool counts and duration histograms |
| Hooks | Actual delivered session-start/end records with matching session/project identity; queued events alone do not satisfy delivery |
| Content excluded | Both content switches exercised through `terma connect`; prompt/reply markers absent from **all** exports; Claude tool content absent; Codex tool output excluded (Codex still exports arguments) |
| Per-repository route (Claude) | `install-content` / `install-redacted`: `terma install` points Claude's user-level exporter at the local relay, which forwards the repository's claimed session to the receiver under the project's policy — install's default (prompts and responses sent), and `--prompts off --exclude-tool-content`, which the relay withholds. |
| Failures | Claude HTTP 400: native API-error fields, ERROR request span and StopFailure delivery |

`test/live/golden/{claude,codex}/telemetry-{content,redacted}.json` lists the required
field names per controlled log/span/metric surface. Every matching record is
checked, so a healthy first record cannot mask a later record missing fields.
Missing surfaces, missing baseline files and disappeared fields fail on every
selected version. New fields are listed in the coverage report for review.
Existing authenticated-scenario goldens remain separate because account fields
and first-party request metadata differ from a loopback provider.

The four telemetry baselines were recorded from Claude 2.1.283 and Codex 0.158.0.
Re-record only after reviewing a deliberate contract change; semantic assertions
must pass before a baseline is written:

```sh
LIVE_UPDATE_GOLDEN=1 TERMA_ENV=dev make telemetry
```

The suite waits up to 45 seconds for the complete contract, including detached
hook delivery. Failure diagnostics show queued event names and whether the
session-end hook ran. Offline mutation tests demonstrate failures for missing
signals, wrong keys, unrelated sessions, broken later records, missing/incorrect
metric values, malformed histogram buckets and broken trace parents.

## Scope and known limits

These scenarios exercise CLI exporters configured through `terma connect`, and
Claude's per-repository route through `terma install` and the local relay. They do not cover
Desktop/app-server launch paths, Codex's per-repository route (runtime `-c` overrides),
or install signing in: the routed scenarios use a server key and a stand-in for the API
gateway's `/v1/identity`, with the key on file from an earlier connect.
They verify the OTLP boundary, not backend parsing or storage. They do not claim
coverage of every conditional vendor event: hosted tools, WebSockets, MCP,
subagents, compaction, user-denied approvals and subscription quota changes need
their own triggering scenarios. Existing live tests cover file attribution,
commits, authenticated usage and selected quota/account evidence.

Repeated local validation also caught an intermittent missing Codex session-end
event: native logs, traces and metrics arrived, but no SessionEnd hook payload
was captured and no end event was queued or delivered within 45 seconds. The
configured hook is synchronous with Codex's maximum three-second timeout. The
lifecycle assertion remains strict locally and in the nightly live workflow; the
suite is not claimed to be reliably green until the missing invocation is fixed.
Pull-request CI sets `TERMA_LIVE_KNOWN_UPSTREAM=codex-session-end`, which drops
only Codex's session end from the contract (the start stays required) and logs
`KNOWN UPSTREAM` when it did not arrive, so the race does not fail unrelated pull
requests at random.
The [Codex shutdown investigation](CODEX-SESSION-END.md) reproduces this with
shell-builtin marker hooks, independent of Terma, and traces the conflicting
upstream cleanup deadlines.

Codex 0.158.0 did not reliably deliver native error logs or metrics when a
controlled terminal HTTP 400 caused it to exit. A controlled 503 followed by a
successful transport retry emitted only the final 200 request log/metric.
Recoverable SSE-error probes also lacked the expected failed-attempt evidence.
These are observed gaps: **Codex failure delivery and per-attempt telemetry are
not covered by a passing contract**. The success contracts must not be read as
proof of those paths. Likewise, an accepted tool's permission decision does not
prove the user-denied path. Raw API-body logging is not enabled by Terma and is
not part of these contracts.

Codex sends separate timing and usage records with the same
`codex.sse_event` / `response.completed` name. Usage checks distinguish these
shapes and require the usage records, rather than counting timing records as
zero-token calls. Codex event time lives in `event.timestamp`; Claude also sets
the OTLP record timestamp. Claude is routed by its project key, while Terma's own
hook logs carry project resource attributes and Codex stamps project attributes
on spans.

Provider references: [Claude telemetry](https://code.claude.com/docs/en/monitoring-usage)
and [Codex telemetry](https://learn.chatgpt.com/docs/config-file/config-advanced#observability-and-telemetry).
The contracts use observed wire shapes; documentation alone is not evidence that
a field was emitted.
