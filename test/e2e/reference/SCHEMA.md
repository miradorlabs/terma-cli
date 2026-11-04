# Harness Reference: published data, schema version 1

What each coding-agent harness build does, observed by running it, and what changed from
one build to the next. The nightly `live.yml` run captures every tested build
(`test/e2e/profile.go`, `report/profiles.json`), `refgen` normalizes each into a snapshot,
compares it with the build before, and publishes the result beside the compatibility matrix
on the `compat-matrix` branch, under `docs/harnesses/`. Everything here is public:
https://raw.githubusercontent.com/miradorlabs/terma-cli/compat-matrix/docs/harnesses/index.json

Every file carries `"schema_version": 1`. A field may be added within a version; nothing is
renamed or removed without bumping it. Absent and `null` mean "not observed", never "none":
an empty list or object means observed and empty.

## Layout

```text
docs/harnesses/index.json                          every harness, its versions, newest first
docs/harnesses/<harness>/index.json                one harness: every version with its change summary
docs/harnesses/<harness>/versions/<version>.json   a build's snapshot
docs/harnesses/<harness>/diffs/<from>..<to>.json   what changed from one build to the next
docs/harnesses/<harness>/prompts/<version>.md      the build's system prompt, as text
docs/harnesses/<harness>/changelog.json            what the vendor's changelog says of each version
docs/harnesses/<harness>/timeline.json             every step's changes, without their values
docs/harnesses/refdiff.wasm, wasm_exec.js          the comparison of any two versions, for a browser
```

`<harness>` is a stable id: `claude`, `codex`, `gemini`, `opencode`, `pi`, `omp`, `hermes`,
`dsh`, `copilot`, `t3`, `claude-desktop`, `codex-desktop`. Versions compare as the harness
numbers them (semver where it is semver); `..` joins the two in a diff's name.

## index.json

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-04T05:02:11Z",
  "harnesses": [
    {
      "id": "claude",
      "name": "Claude Code",
      "vendor": "Anthropic",
      "homepage": "https://claude.com/claude-code",
      "source": null,
      "changelog": "https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md",
      "latest": "2.1.318",
      "support": "supported",
      "versions_tracked": 41,
      "first_tracked": "2026-09-15",
      "last_change": { "from": "2.1.317", "to": "2.1.318", "at": "2026-11-04T04:41:09Z", "changes": 7,
                       "categories": { "prompt": 2, "tools": 1, "telemetry": 4 } }
    }
  ]
}
```

`source`, where the harness's source is public: `{ "repo": "openai/codex", "url":
"https://github.com/openai/codex", "language": "rust" }`.

`support` is terma's verdict on the newest build: `supported` (every capability check it ran
passed), `degraded` (some failed), `unsupported` (the core telemetry check failed),
`untested` (none ran).

## <harness>/index.json

The harness's entry from `index.json`, and every version tracked, newest first:

```json
{
  "schema_version": 1,
  "harness": { "...": "as in index.json" },
  "versions": [
    {
      "version": "2.1.318",
      "released_at": "2026-11-03T22:10:00Z",
      "captured_at": "2026-11-04T04:41:09Z",
      "platforms": ["darwin/arm64", "linux/amd64"],
      "support": "supported",
      "previous": "2.1.317",
      "changes": 7,
      "categories": { "prompt": 2, "tools": 1, "telemetry": 4 },
      "impact": { "breaking": 0, "degrading": 0, "notable": 3, "informational": 4 },
      "undocumented": 5,
      "highlights": ["System prompt changed (+212 −40 chars)", "New tool: Monitor", "New telemetry event: tool_decision"],
      "signing": "valid"
    }
  ]
}
```

`released_at` is the registry's publish time; `captured_at` when a snapshot of it was last
taken. `previous` is the build the diff compares it with, null for the first tracked.
`undocumented` counts changes the vendor's changelog for the version does not mention (null
where there is no changelog to read). `signing` is Gatekeeper's verdict on the build's macOS
code signature when last checked (`valid`, `revoked`, `rejected`, `unsigned`), null where it
was not checked.

`binary.signing` (macOS builds that are Mach-O executables) says who signed the build: its
Developer ID authority chain, team, the leaf certificate (serial, SHA-256, validity), the
signature's secure timestamp, whether it is notarized, and Gatekeeper's verdict when
`checked_at`. Revocation is a fact of when it was checked, not of the build: a certificate
revoked later turns its builds' status to `revoked`. `resigned` marks a build whose revoked
signature was replaced by an ad hoc one so that it would run for the capture; what it was
signed with is read before.

## versions/<version>.json: a snapshot

Each section is what one kind of capture saw. A section the night did not capture is absent.
Text is normalized, so two snapshots differ only where the builds do. The run's own literals
become `<repo>`, `<home>`, `<sandbox>`, `<workdir>`, `<claude-config>`, `<codex-home>`,
`<terma-config>`, `<provider>` (the fixture's URL) and `<prompt>` (the scenario's own
prompt), and a path's slug form (every character but letters and digits a dash, as Claude
Code names a project) `<repo-slug>` and so on. What varies by itself becomes `<version>` (the
build's own version where it stands alone, as in a user agent), `<commit>`, `<date>`,
`<timestamp>`, `<uuid>`, `<hex>` (32 to 64 hex digits), `<tmp>`, `<local>` (a loopback
address and port), `<timezone>` and `<os-release>`. A date inside a name (a beta's, a
model's) is part of the name and stays.

```json
{
  "schema_version": 1,
  "harness": "claude",
  "version": "2.1.318",
  "platform": "darwin/arm64",
  "captured_at": "2026-11-04T04:41:09Z",
  "capture": {
    "command": ["claude", "-p", "<prompt>", "--output-format", "json", "--max-turns", "1"],
    "partial": false,
    "notes": ["Model requests answered by a local fixture provider, not the vendor's API."],
    "sections": { "prompt": "2026-11-04T04:41:09Z", "support": "2026-11-04T05:02:11Z" }
  },

  "model": {
    "default": "claude-opus-5-5",
    "max_tokens": 128000,
    "params": { "thinking": { "type": "adaptive", "display": "omitted" }, "output_config": { "effort": "medium" } },
    "aliases": { "sonnet": "claude-sonnet-5-5", "haiku": "claude-haiku-5-5" },
    "prices": { "claude-opus-5-5": { "input": 15.0, "output": 75.0, "cache_read": 1.5, "cache_write_5m": 18.75, "cache_write_1h": 30.0, "unit": "USD per million tokens" } }
  },

  "prompt": {
    "system": [ { "text": "You are Claude Code…", "chars": 6008, "sha256": "…", "cache": { "type": "ephemeral", "ttl": "5m" } } ],
    "context": [ { "role": "user", "text": "<system-reminder>…", "chars": 1439 } ],
    "chars": 7447,
    "sha256": "…",
    "interactive": { "system": [ "…same shape, the interactive session's…" ], "context": [ ], "chars": 7012, "sha256": "…", "model": "claude-opus-5-5", "tools": ["Agent", "Bash"] }
  },

  "tools": [
    { "name": "Bash", "description": "Executes a bash command…", "input_schema": { "type": "object" }, "sha256": "…" },
    { "name": "spawn_agent", "namespace": "collaboration", "type": "custom", "description": "…", "sha256": "…" }
  ],

  "caching": {
    "breakpoints": [ { "at": "system[1]", "type": "ephemeral", "ttl": "5m" }, { "at": "messages[-1]", "type": "ephemeral", "ttl": "5m" } ],
    "prompt_cache_key": false,
    "across_turns": { "turns": 3, "breakpoints": [ ["system[1]", "messages[0].content[1]"], ["system[1]", "messages[2].content[0]"] ], "stable_cache_key": true, "stable_prefix_chars": 41200 }
  },

  "request": {
    "endpoint": "POST /v1/messages",
    "wire": "messages",
    "query": "beta=true",
    "headers": { "anthropic-version": "2023-06-01", "user-agent": "claude-cli/2.1.318 (external, sdk-cli)", "x-app": "cli" },
    "betas": ["claude-code-20250219", "context-management-2025-06-27", "prompt-caching-scope-2026-01-05"],
    "body_keys": ["context_management", "max_tokens", "messages", "metadata", "model", "output_config", "stream", "system", "thinking", "tools"],
    "stream": true,
    "identifiers": ["metadata.user_id", "header:x-claude-code-session-id"]
  },

  "side_requests": [
    { "method": "HEAD", "path": "/api/hello", "when": "before the first model request" },
    { "method": "POST", "path": "/v1/messages", "model": "claude-haiku-5-5", "tools": 0, "system": [ "…" ], "when": "after the first model request" }
  ],

  "behavior": {
    "retries":      { "statuses": { "529": { "attempts": 4, "failed": 3, "backoff_ms": [2010, 2004, 2012], "honors_retry_after": true, "recovered": true } }, "fallback_model": "claude-sonnet-5-5" },
    "compaction":   { "triggered_at_tokens": 160000, "prompt": "Your task is to create a detailed summary…", "model": "claude-opus-5-5" },
    "tool_output":  { "limit_chars": 30000, "truncation": "middle", "marker": "… [N characters truncated] …" },
    "subagents":    { "model": "claude-opus-5-5", "system": [ "…" ], "tools": ["Bash", "Read"] },
    "sandbox":      { "default_mode": "workspace-write", "approval": "on-request" }
  },

  "telemetry": {
    "surfaces": {
      "logs/api_request": { "keys": { "cost_usd": { "kinds": ["number"], "class": "safe" } } },
      "metrics/claude_code.token.usage": { "keys": { "type": { "kinds": ["text"], "class": "safe" } }, "type": "sum", "unit": "tokens", "temporality": "delta" },
      "traces/claude_code.llm_request": { "keys": { }, "parent": "traces/claude_code.interaction" }
    },
    "export": { "protocol": "http/protobuf", "interval_ms": { "logs": 5000, "metrics": 60000 } }
  },

  "hooks": { "events": { "PreToolUse": { "keys": ["hook_event_name", "session_id", "tool_input", "tool_name"] } } },

  "cli": {
    "help": { "--help": "Usage: claude [options] [command] [prompt]…" },
    "flags": ["--model", "--print", "--resume"],
    "subcommands": ["config", "mcp", "update"]
  },

  "config": { "env_vars": ["ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"], "settings_keys": ["model", "permissions.allow"] },

  "network": { "egress": [ { "host": "statsig.anthropic.com", "method": "POST", "path": "/v1/rgstr", "body_keys": ["events"], "when": "startup" } ] },

  "storage": {
    "transcript": { "path": "<claude-config>/projects/<project>/<session>.jsonl", "format": "jsonl", "record_types": { "assistant": ["message", "uuid", "parentUuid"] } }
  },

  "binary": {
    "runtime": "node v26.3.0", "sdk": { "@anthropic-ai/sdk": "0.127.0" }, "size_bytes": 212000000,
    "signing": {
      "format": "developer-id",
      "authority": ["Developer ID Application: OpenAI, L.L.C. (2DC432GLL2)", "Developer ID Certification Authority", "Apple Root CA"],
      "team_id": "2DC432GLL2", "identifier": "codex", "signed_at": "25 Jan 2026 at 17:11:12", "hardened_runtime": true,
      "certificate": { "subject": "Developer ID Application: OpenAI, L.L.C. (2DC432GLL2)", "serial": "68A68AF8A434EE4FDE46F8680B23BDCC",
                       "sha256": "9935b17b…", "not_before": "2024-01-23T06:19:53Z", "not_after": "2029-01-23T06:19:52Z" },
      "status": "revoked", "notarized": false, "assessment": "CSSMERR_TP_CERT_REVOKED",
      "checked_at": "2026-11-04T04:35:53Z", "resigned": true
    }
  },

  "support": {
    "status": "supported",
    "capabilities": [ { "id": "relay.telemetry", "label": "Telemetry reaches the project through the local relay", "result": "pass", "platform": "darwin/arm64" } ]
  }
}
```

## diffs/<from>..<to>.json

```json
{
  "schema_version": 1,
  "harness": "claude",
  "from": "2.1.317",
  "to": "2.1.318",
  "generated_at": "2026-11-04T05:02:11Z",
  "source": { "compare": "https://github.com/openai/codex/compare/rust-v0.159.0...rust-v0.159.1" },
  "changelog": { "url": "…", "text": "- Added Monitor tool\n- Fixed …" },
  "summary": {
    "changes": 7,
    "categories": { "prompt": 2, "tools": 1, "telemetry": 4 },
    "impact": { "breaking": 0, "degrading": 0, "notable": 3, "informational": 4 },
    "undocumented": 5
  },
  "changes": [
    {
      "id": "prompt.system[1]",
      "category": "prompt",
      "op": "changed",
      "path": ["prompt", "system", "1", "text"],
      "title": "System prompt changed",
      "impact": "notable",
      "before": null,
      "after": null,
      "text_diff": "@@ -12,7 +12,9 @@\n context\n-old line\n+new line\n",
      "stats": { "added_chars": 212, "removed_chars": 40 },
      "terma": null,
      "source": { "refs": [ { "path": "codex-rs/core/prompt.md", "line": 14, "url": "…" } ], "gone": false },
      "documented": false
    }
  ]
}
```

A change:

| field | |
|---|---|
| `id` | stable within the diff: the dotted path, list items by their key (`tools.Bash.description`) |
| `category` | the snapshot section: `model`, `prompt`, `tools`, `caching`, `request`, `side_requests`, `behavior`, `telemetry`, `hooks`, `cli`, `config`, `network`, `storage`, `binary`, `support` |
| `op` | `added`, `removed` or `changed` |
| `path` | where in the snapshot, as segments; list items by their key |
| `title` | one line a person reads |
| `impact` | `breaking` (a terma capability failed that passed on `from`), `degrading` (terma loses data it read), `notable` (prompt, tools, model, caching, network, a removed field), `informational` (the rest) |
| `before`, `after` | the values, for anything short; null where `text_diff` says it |
| `text_diff` | a unified diff, for text over one line (prompts, descriptions, help) |
| `stats` | for text: characters added and removed |
| `terma` | `{ "capabilities": ["relay.telemetry"] }` where a capability's result changed with it |
| `source` | where the harness's public source names the thing changed, at `to` (for one removed, `gone` and where `from` named it) |
| `documented` | whether the vendor's changelog for `to` mentions it; null without a changelog. A heuristic: a named thing (a tool, field, flag, beta, model) is documented where the changelog names it; a prompt's change where it says "system prompt"; a text's change (a tool's description) where it names the thing and its wording ("description", "prompt", "instruction"). `undocumented` counts the rest, the history's verdicts aside |

A diff compares only the sections both snapshots have: a build captured before a kind of
capture existed is no evidence the next build added everything that capture sees. `sha256`
and `chars` are derived and never read as changes. The changelog is the vendor's markdown
changelog section for `to` (Claude Code, Copilot CLI) or the release notes of its tag
(harnesses whose source is public).

Lists are compared by key: tools by `name`, telemetry by surface then key, hooks by event,
egress by host, method and path, capabilities by `id` and platform. Strings of more than one
line are compared as text.

## <harness>/changelog.json

What the vendor's changelog says of each version, where it says something that could be read
(a version absent has none): the comparison of any two versions judges `documented` against
every one between them.

```json
{ "schema_version": 1, "harness": "claude",
  "versions": { "2.1.318": { "url": "https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md", "text": "- Added …" } } }
```

## <harness>/timeline.json

Every step between consecutive versions, newest first: each change's `id`, `category`, `op`,
`title`, `impact` and `documented`, without values. Over a range of versions it says which
version made each change (a net change matches its steps by `id`).

```json
{ "schema_version": 1, "harness": "claude",
  "steps": [ { "version": "2.1.318", "previous": "2.1.317",
               "changes": [ { "id": "tools.Monitor", "category": "tools", "op": "added", "title": "New tool: Monitor", "impact": "notable", "documented": true } ] } ] }
```

## Comparing any two versions

Diffs are published between consecutive versions only; any other two are compared where they
are read, by the same code that made the published ones (`refdiff`), compiled to WebAssembly:
`refdiff.wasm` with the Go runtime that built it, `wasm_exec.js`. After

```js
const go = new Go();
const { instance } = await WebAssembly.instantiateStreaming(fetch("refdiff.wasm"), go.importObject);
go.run(instance);
```

`harnessReferenceCompare(fromSnapshotJSON, toSnapshotJSON, changelogJSON)` returns, as a
string, a diff in the schema above between the two snapshots (`from` and `to` the two
versions, net of every step between them), each change `documented` where the changelog of
any version after `from` up to `to` mentions it; or `{"error": "…"}`. It needs a page's
CSP to allow `'wasm-unsafe-eval'`.

## Links

The reference's pages, which announcements and anything else link to:

| page | |
|---|---|
| `https://terma.ai/h/<harness>/<version>` | a version's page: its changes from the version before, and the vendor's changelog of it (redirects to `/harnesses/<harness>/<version>`) |
| `https://terma.ai/harnesses/<harness>/compare/<from>...<to>` | any two versions compared |

## Announcements

Not published: what `refgen -announce` writes for the workflows to post, each version newer
than any of its harness captured before, once (a backfill, a failed capture and a harness's
first publication announce nothing). Each has the version, the one before, its page and
comparison links, counts, highlights, its signing verdict, `text` (a post of at most 280
characters, a link counted as 23) and `slack` (a Slack message with buttons to the page and
the comparison).
