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
docs/harnesses/recent.json                         the newest versions across every harness, for a feed
docs/harnesses/<harness>/index.json                one harness: every version with its change summary
docs/harnesses/<harness>/versions/<version>.json   a build's snapshot
docs/harnesses/<harness>/diffs/<from>..<to>.json   what changed from one build to the next
docs/harnesses/<harness>/prompts/<version>.md      the build's system prompt, as text
docs/harnesses/<harness>/changelog.json            what the vendor's changelog says of each version
docs/harnesses/<harness>/timeline.json             every step's changes, without their values
docs/harnesses/<harness>/lineage.json              since which version each command, option, setting and variable is there
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
      "last_change": { "from": "2.1.317", "to": "2.1.318", "at": "2026-11-04T04:41:09Z", "released_at": "2026-11-03T22:10:00Z", "changes": 7,
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

  "hooks": {
    "supported": ["Notification", "PostToolUse", "PreToolUse", "SessionStart", "Stop"],
    "source": "binary",
    "events": {
      "PreToolUse": {
        "fired": true, "count": 11,
        "keys": ["cwd", "hook_event_name", "session_id", "tool_input", "tool_input.command", "tool_name"],
        "kinds": { "cwd": "text", "hook_event_name": "text", "session_id": "text", "tool_input": "map", "tool_input.command": "text", "tool_name": "text" },
        "example": { "cwd": "<repo>", "hook_event_name": "PreToolUse", "session_id": "<session>", "tool_input": { "command": "printf hooks-ok" }, "tool_name": "Bash" },
        "stdin": "json",
        "env": ["CLAUDECODE", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PROJECT_DIR"],
        "env_paths": { "CLAUDE_PROJECT_DIR": "<repo>" },
        "env_values": { "CLAUDECODE": "1", "CLAUDE_CODE_MESSAGING_TOKEN": "<redacted>", "CLAUDE_PROJECT_DIR": "<repo>" },
        "cwd": "<repo>",
        "matcher": true,
        "control": { "exit_2": "blocks the tool; message to the model", "json_decision": "blocks the tool; message to the model" }
      }
    },
    "not_fired": { "Notification": "tried: an interactive session's permission prompt left unanswered; the build notified of none" }
  },

  "cli": {
    "help": { "--help": "Usage: claude [options] [command] [prompt]…", "exec -h": "…" },
    "commands": {
      "codex exec": {
        "usage": ["Usage: codex exec [OPTIONS] [PROMPT]"],
        "description": "Run Codex non-interactively",
        "helped": true,
        "options": {
          "--sandbox": { "names": ["-s", "--sandbox"], "value": "<SANDBOX_MODE>", "description": "Select the sandbox policy…",
                         "choices": ["read-only", "workspace-write", "danger-full-access"] },
          "--color": { "names": ["--color"], "value": "<COLOR>", "description": "…", "default": "auto", "choices": ["always", "never", "auto"] }
        },
        "arguments": { "PROMPT": { "description": "Initial instructions for the agent…" } }
      },
      "codex exec resume": { "description": "Resume a previous session by id…", "helped": false }
    }
  },

  "config": {
    "source": "codex-rs/core/config.schema.json at rust-v0.159.1",
    "options": {
      "sandbox_mode": { "type": "string", "description": "Sandbox mode to use.", "enum": ["read-only", "workspace-write", "danger-full-access"] },
      "sandbox_workspace_write.network_access": { "type": "boolean", "default": false },
      "mcp_servers.<name>.command": { "type": "string" },
      "personality": { "type": "string", "deprecated": true }
    },
    "env_source": "the variables its source reads",
    "env_vars": ["CODEX_HOME", "OPENAI_API_KEY"]
  },

  "network": { "egress": [ { "host": "statsig.anthropic.com", "method": "POST", "path": "/v1/rgstr", "body_keys": ["events"], "when": "startup" } ] },

  "storage": {
    "transcript": { "path": "<claude-config>/projects/<project>/<session>.jsonl", "format": "jsonl", "record_types": { "assistant": ["message", "uuid", "parentUuid"] } }
  },

  "filesystem": { "linux/amd64": { "paths": { "<claude-config>/.claude.json": { "ops": ["read", "write"], "phases": ["startup"] } },
                                   "exec": { }, "decoys": { "<home>/.aws/credentials": "not touched" }, "sockets": { }, "…": "see filesystem below" } },

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

### live

The published first turn runs with the feature flags the build ships with: it is taken behind
a local proxy that answers every request but the model's empty, so the build's flag service,
bootstrap and the like return nothing and it falls back to its defaults (`capture.notes` says
so). `live` is the same turn as it ran with the vendor's live server-side flags that night:

```json
"live": { "prompt": { "system": [ … ], "context": [ … ], "chars": 7012, "sha256": "…" },
          "model": "claude-opus-5-5", "tools": ["Agent", "Bash", "Monitor"], "differs": true }
```

`differs` says whether the live flags changed the prompt. A rollout changes `live` from night to
night whatever the build, so it is no section: it is never compared, and a diff never says it.
Absent where there was no proxied turn to compare it with.

### hooks

Every hook event the build knows, and what it hands a hook when one fires (the hooks probe,
`test/e2e/hooks_probe.go`). `supported` is read from the build itself, never from a list of
the suite's alone: Claude Code's settings schema's event enum, the input schemas Codex embeds,
Gemini CLI's event enum, the schema Copilot CLI's packed script embeds. `source` says where:
`binary` (its executable or package), `source` (its public source at its tag) or `seed` (the
suite's own list, where the build named none it could read).

The probe registers a recording hook for every supported event, in place of terma's, and runs
what fires each one: tool calls that succeed, fail and are denied, a subagent, one in a
worktree of its own, a compaction, a resumed session, a custom command, a turn the provider
refuses; for Claude Code also tasks and a teammate (the task tools and agent teams turned on in
the sandbox), an MCP server whose tool asks the user a question, and an interactive session in a
terminal (`/add-dir`, a settings file and a watched file changed from outside, a permission
prompt left unanswered, the session left idle). Each event that fired has:

| field | |
|---|---|
| `fired`, `count` | whether it fired, and how many times over the probe's runs |
| `keys`, `kinds` | its payloads' key paths, four deep, a list's items' under `[]` (`tool_calls[].tool_name`); the kind of value each held (`text`, `number`, `bool`, `list`, `map`, `null`), joined with `\|` where runs differed |
| `example` | one payload whole, with its values: the one with the most key paths. The run's paths, session, ids and times are placeholders (`<repo>`, `<session>`, `<uuid>`, `<timestamp>`); a value whose key or variable name says it is a secret (a token, key, password), or that has a credential's shape, is `<redacted>`; a path of the machine's outside the sandbox (a home folder, what a package manager installed) is `<machine-path>`, the machine's and its user's names `<host>` and `<user>`, the PATH `<path>`; a text over 2,000 characters is cut there, ending `…<cut: N more characters>`. A JSON string where the hook's standard input was text; absent where it was empty |
| `stdin` | what the hook's standard input held: `json`, `text` or `empty` |
| `env`, `env_paths` | the environment variables the harness set for the hook (the ones it was started with, and the sandbox's own settings, left out); the value of each that is one path in the sandbox, as its placeholder |
| `env_values` | the value of each of `env`, as the event's first run had it, scrubbed as `example` is: a secret's `<redacted>`, a list of folders `<path>` |
| `cwd` | the directory its first run ran in |
| `args` | how many arguments the harness added to the hook's command line, where it added any |
| `matcher` | whether it takes a matcher: measured, with a second hook whose matcher matches nothing, which runs where the event ignores matchers |
| `control` | what the build did with a hook's answer, where the probe tried it (Claude Code and Codex: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `Stop`): `exit_2` (exit status 2 with a message on stderr), `json_decision` (a JSON deny or block), `additional_context` (context to add). Each says whether the build acted on it (`blocks the tool`, `blocks the prompt`, `keeps the turn going`) and whether the message reached the model; `additional_context` `reaches the model` or is `ignored` |

`not_fired` says, for each supported event no run fired, why: what the probe tried, where it
tried something the build did not answer. A build captured before the hooks probe existed has
only `events`, each with its `keys` and `count`: the payloads of the hooks its plain first turn
ran; one captured before examples has no `example` or `env_values`.

`trust` is there for a build that runs a hook only once the developer has trusted it (Codex
from 0.129). The probe trusts its hooks the way Codex records a developer's review of them: the
build's own app-server lists each hook's key and the hash it gives it (`hooks/list`), and each
goes into the sandbox's `config.toml` as `[hooks.state."<key>"] trusted_hash`. `codex exec`
before 0.141 does not carry `--dangerously-bypass-hook-trust` into its threads, so there the
hooks run only trusted. `trusted` says every hook was; `untrusted` that the build would not
trust them, and none of its events can fire. A Codex 0.129–0.140 snapshot captured before the
probe trusted its hooks has no `trust`, and fired nothing.

In a diff, a new event in `supported` is notable and one gone degrading; an event no longer
fired is degrading; a payload field or environment variable gone, and a control point's result
changed, notable. A field's kind or a variable's path added or removed with it is not a change
of its own, and neither is `not_fired` or `trust`. `example` and `env_values` are the run's values and are
never compared: what a build hands a hook is its keys, their kinds and the variables' names.
`method` is how the probe made events fire (2: the task, MCP and worktree runs and the
interactive session, the first to publish `example`; a snapshot without it is 2 where its events
have examples, else 1). Two builds probed by different methods fired different tools in
different terminals, so their `events` are not compared at all; `supported`, read from the
build, still is.

In `binary.signing`, the certificate is compared as one thing (a new certificate is one change),
and `signed_at`, `checked_at`, `assessment`, `resigned` and `authority` are never read as
changes: they differ by nature, or say again what `status` and the certificate say.

### docs

What the vendor's own reference said of the build's configuration, the night the build was the
newest: Claude Code's settings reference and environment variable pages
(`code.claude.com/docs/en/settings-reference.md`, `env-vars.md`). A reference describes the
vendor's current release, not every build, so `docs` is kept with the build that was newest when
it was read, and older builds keep theirs: it is no section, and never compared.

```json
"docs": {
  "source": ["https://code.claude.com/docs/en/settings-reference.md", "https://code.claude.com/docs/en/env-vars.md"],
  "fetched_at": "2026-11-04T05:02:11Z",
  "settings": { "advisorModel": { "description": "Pick which model answers…", "type": "string, one of the aliases…",
                                  "default": "unset, so the advisor is off", "scope": "Any file", "topic": "Model and responses", "read": true } },
  "undocumented_settings": ["quietHours", "breakReminder"],
  "documented_not_read": ["idleCompaction"],
  "env_vars": { "ANTHROPIC_API_KEY": { "description": "API key sent as X-Api-Key header…", "read": true } },
  "undocumented_env_vars": ["CLAUDE_CODE_…"],
  "documented_env_not_read": ["…"]
}
```

`read` says whether the build reads the setting (its `config.options`) or variable (its
`config.env_vars`). `undocumented_settings` are the top-level settings the build reads that the
reference does not mention; `documented_not_read` those it documents that the build was not
found to read (a setting newer than the build, or one the build's schema builds where it cannot
be read), the reference's `Global config` keys (kept in `~/.claude.json`, outside the settings
schema) aside. The variable lists are as good as `config.env_vars`, a heuristic for a closed
harness: a name the build composes at run time is not found, and one it only sets may be.

### network

`egress` lists each request the build made to anything but its model provider, through the
recording proxy (`method`, `host`, `path`, `body_keys`, `when`: before, during or after its
model calls; `error` where the proxy could not read it, a TLS handshake refused). Every request is
answered locally and empty, never forwarded: this is what the build asks for when its servers
answer nothing.

`telemetry` is the build's own telemetry to its vendor, read from those requests' bodies
(Claude Code's `event_logging` batches, Gemini CLI's Clearcut log), by endpoint:

```json
"telemetry": { "POST api.anthropic.com/api/event_logging/v2/batch": {
  "events": { "tengu_deferred_tools_pool_change": { "count": 1, "fields": ["device_id", "env", "env.platform", "event_name", "session_id"] } },
  "unobserved": ["tengu_retention_sweep"] } }
```

Event names, how many times the run reported each, and their fields two keys deep; never their
values. `count` is the run's and is not compared. `unobserved` are events builds before and after
this one reported there and its run did not (one reported only sometimes): not compared as
removed and added. Two builds where one's interception failed (an egress `error` the other has
not) are not compared in `network`.

### filesystem

What the build does on the file system and with sockets, and which programs it starts, traced
while it runs a turn (the files profile, `test/e2e/profile_files_test.go`, `filetrace.go`), by
platform: `linux/amd64` traced with strace, `darwin/arm64` with Endpoint Security's
`eslogger` (as root, with Full Disk Access) and `lsof`. Where neither can run the section is
absent; a night that traced one platform keeps the other's from before. Each platform's is the
platform's own build of the harness, so two platforms differ where the builds do. The turn is the profile's first turn, behind the same recording proxy as the published
one, then a tool turn: a shell command (`git status --short && git log --oneline -1`) and a
file written in the repository (Claude Code's `Write`; Codex's shell, `--sandbox
workspace-write`).

The run is a clean room: nothing of the capturing machine's own is in it, so nothing of it is
traced as the build's. No terma: no setup, and none of terma's hooks, relay or exporter settings.
HOME and the harness's config folders are the sandbox's own, with the decoys planted in them,
and the build reaches the network only through the recording proxy. `SHELL` is the OS's default
login shell, `/bin/zsh` on macOS and `/bin/bash` on Linux: HOME being the sandbox's, no rc file
of a developer's loads, while the system's own do, as for every login shell (macOS's
`/etc/zprofile` runs `path_helper`, Debian's `/etc/profile` runs `id -u`), so those are part of
what the build's shells do on a stock machine. The build runs as it does with hooks configured: a
recording hook is registered for every hook event it knows (the hooks probe's recorder), and the
trace drops it, its shell, its process and its files, in `dropped.hooks`. What the build hands a
hook is the [hooks](#hooks) section's. A platform's trace made before the clean room, until a
later one replaces it, ran with terma's hooks installed: its `exec` lists `terma hook …` and the
shell that started each, its `dropped` has `terma` (what terma's hooks did after they started)
and no `hooks`, and its commands have no `parents`, `role` or `purpose`.

```json
"filesystem": {
  "linux/amd64": {
    "platform": "linux/amd64", "tracer": "strace",
    "paths": {
      "<claude-config>/.claude.json": { "ops": ["stat", "read", "write"], "phases": ["startup", "turn"] },
      "<repo>/.git/index": { "ops": ["read", "write"], "phases": ["turn"], "programs": ["git"] },
      "/etc/claude-code/managed-settings.json": { "ops": ["probe"], "phases": ["startup"] }
    },
    "exec": {
      "rg --no-config --files --hidden <repo>": { "argv": ["rg", "--no-config", "--files", "--hidden", "<repo>"], "phases": ["startup"], "count": 1, "by": "claude",
        "parents": [], "role": "harness", "purpose": "lists the repository's files" },
      "/bin/bash -c -l SNAPSHOT_FILE=<claude-config>/shell-snapshots/snapshot-bash-<n>-<id>.sh …": { "argv": ["/bin/bash", "-c", "-l", "SNAPSHOT_FILE=…"], "phases": ["turn"], "count": 1, "by": "claude",
        "parents": [], "role": "harness", "purpose": "the shell snapshot: captures the login shell's environment for the shell tool" },
      "cat": { "argv": ["cat"], "phases": ["turn"], "count": 1, "by": "bash",
        "parents": ["/bin/bash -c -l SNAPSHOT_FILE=<claude-config>/shell-snapshots/snapshot-bash-<n>-<id>.sh …"], "role": "harness",
        "purpose": "the shell snapshot: captures the login shell's environment for the shell tool" },
      "git status --short": { "argv": ["git", "status", "--short"], "phases": ["turn"], "count": 1, "by": "bash",
        "parents": ["/bin/bash -c source <claude-config>/shell-snapshots/snapshot-bash-<n>-<id>.sh … eval 'git status --short && git log --oneline -1' …"], "role": "tool" }
    },
    "lookups": ["bash", "git", "node", "zsh"],
    "decoys": { "<home>/.aws/credentials": "not touched", "<repo>/.env": "stat", "<home>/.ssh/agent.sock": "not touched" },
    "sockets": {
      "listening": { "tcp 127.0.0.1:<ephemeral>": { "phases": ["startup"], "count": 1 } },
      "connections": { "tcp <provider>": { "role": "provider", "phases": ["startup"], "count": 1 },
                       "udp <resolver>:53": { "role": "dns", "phases": ["startup"], "count": 2 } },
      "unix": { "<tmp>/cc-socks/<n>.sock": { "ops": ["bind"], "phases": ["startup"], "count": 1 },
                "/var/run/nscd/socket": { "role": "nscd", "ops": ["connect"], "phases": ["turn"], "count": 4, "missing": true } },
      "dns": ["statsig.anthropic.com"]
    },
    "macos": { "xpc": { "com.apple.securityd.xpc": { "phases": ["startup"], "count": 1 } },
               "launch_items": { }, "tcc": { }, "task_ports": { } },
    "dropped": { "libraries": 335, "system": 639, "proc": 241, "hooks": 1607, "path-search": 193, "helpers": 12 },
    "unobserved": { "paths": ["<claude-config>/statsig"] }
  }
}
```

| field | |
|---|---|
| `paths` | every path the build touched, normalized as the rest of the snapshot is (`<home>`, `<repo>`, `<claude-config>`, `<codex-home>`, `<tmp>`, `<sandbox>`; `<repo-slug>` where a name is a path's slug; a run's ids, times, process ids and temporary names as `<uuid>`, `<time>`, `<n>`, `<id>`, `<hash>`; `<user-home>` the home of the user the capture ran as, which a program reads through the user database) |
| `ops` | what was done to it, of `probe` (looked for and not there: a failed open or stat, ENOENT), `stat` (its metadata or access checked, a link read), `list` (a folder read), `read` (opened to read), `write` (opened to write, truncated, its times or mode set, renamed into place), `create` (made: a folder, a file created where it was not), `delete`, `rename` (renamed away from), `chdir`, `denied` (EACCES, EPERM, EROFS) |
| `phases` | when: `startup` before the first model request, `turn` from it to the last response, `exit` after |
| `programs` | the programs other than the build that touched it (`git`, `bash`): a program the build starts is recorded for what it touches of the run's own places, those with a placeholder |
| `exec` | each program started, by its normalized command line: `argv`, the phases it ran in, how many times (`count`), the program that started it (`by`: the build, or a shell it started), and `parents`, `role` and `purpose` (below). The hook recorder is never one: it, the shell the build runs its command line in and what it starts are `dropped.hooks` |
| `exec.parents` | the exec keys of the commands that started it, sorted: each is a key of `exec` too, so a reader can follow a program up to the build. An empty list is the build itself; where the build started it as well as a command did, `<build>` is first among them |
| `exec.role` | whose doing it was: `harness` (the build acting on its own: its startup, its context, its shell snapshot), `tool` (the command the scenario's fixture model told a tool call to run, `git status --short && git log --oneline -1` or Codex's `printf … > notes.txt`, and everything around it: the shell, sandbox and helpers between the build and it, whether or not their own command lines show the command whole (strace cuts a long argument short), and the programs it starts), or `both`, a command line that ran as each |
| `exec.purpose` | what it is for, where its command line is a well-known one (`filetrace.go`'s `execPurposes`, the one table of them): the shell snapshot, a Keychain lookup, a process's start time, the repository's state read for the model's context, the platform and package-manager probe, the tool command's sandbox, the repository's file list, a login shell's own startup (`path_helper`, `id -u`). A program with none of its own takes the purpose of the command that started it, where that one's covers what it starts (the `cat` a shell snapshot runs is the snapshot's; what a sandbox runs is not the sandbox's), so a reader can say "`cat`, started by `/bin/zsh -c -l SNAPSHOT_FILE=…` (the shell snapshot: …)". A tool's command takes only the purposes that hold for a tool's too (a sandbox, a login shell's startup): the model's `git status` is not the build reading the repository. Absent where no rule says; presentation, never compared |
| `lookups` | the programs the build itself looked for on its PATH, found or not |
| `decoys` | the planted credentials, each with the most the build did to it: `not touched`, `stat`, `opened` (opened, its content not read), `read`, `connected` (a socket), `written`, `deleted` |
| `sockets.listening` | each address the build listened on (`listen`, or a UDP bind to a port it chose): `proto host:port`, a port it let the system pick `<ephemeral>` (strace: bound to port 0; lsof: 49152 and up) |
| `sockets.connections` | where it connected: the run's own endpoints by name (`<provider>`, `<proxy>`, `<relay>`, `<receiver>`), the resolver as `<resolver>:53`, any other loopback address as `<local>:port`, anything else `<remote>:port` |
| `sockets.unix` | the Unix sockets it connected to or bound (`ops`), by path or `@abstract` name, with their `role` where the path says it: `decoy ssh-agent`, `decoy docker`, `ssh-agent`, `docker`, `dbus`, `x11`, `wayland`, `nscd`, `syslog`; `missing` where every attempt found none |
| `sockets.dns` | the names its DNS queries asked for, read from the query's bytes |
| `macos` | what only Endpoint Security sees: the XPC services it connected to (`com.apple.securityd.xpc`, the Keychain; `com.apple.tccd`), launch items it added (persistence), TCC permissions it changed, and the task ports of processes outside its own tree it took, by the process's name (a program it starts that reads every process's, as `ps` does, is one `<processes>`: never a list of what the machine runs) |
| `dropped` | how many events were left out, by why (below) |
| `unobserved` | paths, commands and sockets builds before and after this one have on this platform and its run did not (set by refgen) |

**Decoys.** Before the run the scenario plants credentials a developer's machine has, fake: a
throwaway ed25519 key made for the run at `~/.ssh/id_ed25519`, `~/.aws/credentials`,
`~/.netrc`, `~/.config/gh/hosts.yml`, `~/.docker/config.json`, `~/.npmrc`,
`~/.git-credentials`, and a `.env` and `secrets.json` in the repository; and two sockets it
listens on, an SSH agent (`SSH_AUTH_SOCK=~/.ssh/agent.sock`) and Docker
(`DOCKER_HOST=unix://~/.docker/run/docker.sock`), counting who connects. Their contents are
never recorded. On macOS, Endpoint Security has no read event: a decoy opened to read is
`opened` there, never `read`.

**What is dropped**, and why: `proc`, `sys`, `dev` (the kernel's files); `libraries` (shared
libraries and the loader's files, `/lib`, `/usr/lib`, `*.so`, `*.dylib`, macOS's `/System`);
`system` (what every process reads through its C library or runtime: the resolver's and the
user database's files, time zones, locales, certificate authorities, terminal descriptions,
macOS's shared preferences); `self` (the build's own installation: its binary and the folder it
sits in); `hooks` (the recording hook the scenario registers for every hook event: the shell the
build runs its command line in, the recorder's process and everything it does or starts, and
every path under the recorder's folder; the [hooks](#hooks) section says what the build hands a
hook); `terma` (a trace from before the clean room alone: what terma's hooks did after they
started); `test` (the test's own files, the proxy's certificate, the checkout the suite runs
from); `path-search` (looking along the PATH for a
program, which `lookups` says, and an exec that found nothing there); `helpers` (what a program
the build started touched outside the run's own places: its own installation, the system's
files); `sandbox-setup` (bubblewrap building the root of a sandbox before it runs the command in
it, which its command line in `exec` says); `machine` (what the machine has, not what the build does: the programs a
package manager installed, `/opt/homebrew`, `/usr/local/Cellar` and the like, which a build
looking for npm or pnpm finds at whatever versions the machine has; and what the build only
found and looked at in the machine's shared temporary folder: other programs' and sessions' folders it lists as it
cleans up; what it made, read, wrote or removed there, what it looked for and did not find,
and the folders on the way to what it made stay). In the temporary folder a path's `create`
reads as `stat`: whether a path the build makes when it is missing was there already is the
machine's state (an earlier run's), not the build's. `lost` is not the build's: it counts the events
Endpoint Security dropped because eslogger did not read them in time. A trace that lost any is
not published (the section keeps the last whole one): it says nothing of what the build did
not do.

**Sampling.** On macOS TCP and UDP sockets are sampled with lsof every 250 ms over the build's
process tree: a socket opened and closed between two samples is missed (a short HTTP request),
so `connections` there say less than strace's.

In a diff, each platform both builds were traced on is compared, and of it what a build does,
never what a run measured: a path new or gone ("Now reads ~/.gitconfig", "No longer looks for
<repo>/CLAUDE.md") and each op it gained or lost, a command new or gone ("New command at
startup: `git log -n 5`"; one gone and one new that run the same program with at least half the
same arguments are one "Command changed", with the command line's `text_diff`), the phases a
command runs in, who starts it (`by`) and whose doing it is (`role`: "`git status` now runs as
the build's own doing (was part of the tool call)", notable where it becomes the build's own;
a build traced before roles were recorded has none, which is no change), programs looked
for, each decoy's level ("Reads ~/.aws/credentials (a planted decoy)"; "Connects to the decoy
ssh-agent socket"), listeners ("Now listens on 127.0.0.1, an ephemeral port (tcp)"),
connections, Unix sockets, names looked up, and macOS's XPC services, launch items ("Adds a
launch item (runs at login): …"), TCC changes and task ports. A path's phases and programs,
counts, `dropped`, `argv`, and a command's `parents` and `purpose` (presentation) are not
compared, nor terma's hooks in a trace from before the clean room (`terma hook …` and the shell
each ran in): they were the scenario's, not the build's. A macOS trace's paths are not compared at all:
Endpoint Security logs every path a build resolves, and how far a build's background work (Claude
Code's cleanup walk) got before it exited is a race, so one build traced twice lists different
paths; they are each version's to read, and its programs, sockets, decoys and services are
compared. Nor is what a macOS build does after it exits (a command run only at exit, a command's
exit phase, who starts such a command and its role): eslogger's last events can still be on their way when the trace stops. Linux's
trace compares both. The PATH the build ran with is `<path>` wherever a command line holds it (a shell's
snapshot exports it). strace's cut of a long argument ends at its last whole line, marked `…`, and SQLite's
`-wal`, `-shm` and `-journal` files keep no `delete` (removed when the database closes in time).
A path or Unix socket whose name is the run's own
(a lock, a temporary or journal file, a name with `<uuid>`, `<hash>`, `<id>`, `<time>`; a
process id's `<n>` names the same file in every run) touched by one build and not the other is
no change, nor what was done to it, and neither is anything `unobserved` on
either side, nor a decoy planted for one build alone.

Impact: a decoy the build now opens, reads, writes, deletes or connects to is `notable` and
its change's `path` is `["filesystem", <platform>, "decoys", <path>]`, which a page can flag
(and `Highlights` puts first among the notable); so are a new write, create, delete or rename
outside the harness's own folders, the repository and the temporary folder, a new command, a
new program looked for, a new listener, a new connection other than the run's own endpoints, a
new Unix socket (but the resolver cache's and the log's), a new name looked up, a new launch
item, TCC change or task port, and a new XPC service that holds secrets or permissions
(`com.apple.securityd`, `com.apple.SecurityServer`, `com.apple.tccd`, `com.apple.trustd`,
`com.apple.authd`). The rest is `informational`.

### tools and prompt context

A tool a build names but defers (its schema loaded on demand, through its tool search: Claude
Code's from 2.1.304) is in `tools` as `{ "name": "WebFetch", "deferred": true }`; one that
becomes deferred is one change ("WebFetch is now a deferred tool"), not its description and
parameters removed. A system message among a request's turns (Claude Code's environment,
deferred tools and agent types) is in `prompt.context` with `role: "system"`.

`behavior.retries` is kept in each snapshot but not compared: its waits and outcomes move with
the machine's load and with runs the probe cuts short, until it reads the same run to run.

### cli

`help` keeps each help text as the build printed it: every command's `--help`, and its `-h`
where that prints something else. `commands` itemizes them, by command as a person types it:
its usage lines, description, options (by their first long name: every name, the value it
takes, its description, and the default, allowed values and environment variable its help
notes), arguments, and the commands it lists (with their description; `helped` false where its
own help was not read). Descriptions are read as one line a paragraph, however the terminal
wrapped them, so a narrower terminal is no change. A diff lists each command, option and
argument added or removed, each changed description, value, default, alias, environment
variable and allowed value; the full help texts are compared too, for what no entry holds
(examples, notes).

### config

Every configuration option the build reads and every environment variable, with as much as
the build says of each. `options` are by dotted path from the top of the settings file
(`sandbox_workspace_write.network_access`); a table of keys the user names is `<name>`
(`mcp_servers.<name>.command`), a list's items `[]` (`hooks.<name>[].matcher`); paths go four
keys deep. Each option has, where its source says: `type` (its kinds of value, joined with `|`:
`string`, `boolean`, `integer`, `number`, `array`, `object`, `null`), `description`, `default`,
`enum` (the values it takes) and `deprecated`.

`source` is where the options came from, and `env_source` how the variables were read:

| harness | `options` | `env_vars` |
|---|---|---|
| Codex | `codex-rs/core/config.schema.json` at the release's tag, `$ref`s resolved, `oneOf`/`anyOf` a union; before the schema, `ConfigToml`'s fields | what its source reads: `std::env::var`, `var_os`, `env!`, clap's `env =`, and functions that read the name they are given |
| Gemini CLI, Pi | `settings.schema.json` at the tag | `process.env` in its source |
| OpenCode | its Effect Schema config (`ConfigV1.Info`), read across its modules | `process.env` in its source |
| omp | the settings its source `register()`s | `process.env` in its source |
| DeepSeek Harness | `docs/config-catalog.md`, each plugin's config interface under its package's name | `process.env` in its source |
| Hermes | `DEFAULT_CONFIG`: types from its defaults, descriptions from its comments | `os.environ`, `os.getenv` in its source |
| Claude Code | the zod settings schema its bundle declares, read from minified code (heuristic: `binary.heuristic` says so) | `process.env` in its bundle, and its own prefixed names read through what it keeps `process.env` under |
| Copilot CLI | the settings registry its script embeds | `process.env` in its script |

A public harness's are read from its source at refgen time (`refgen -source`), so no capture is
needed; a source's tests, test helpers, build scripts and comments are not the build's. A build
captured before then has `env_vars` from its binary's strings (`env_source` "the names its
strings hold", with `env_vars_uncertain`), and Claude Code's `settings_keys`, the schema's
top-level keys.

In a diff, options compare key by key: a new one is "New setting: …" (notable where its key
guards something: sandbox, permission, approval, allow/deny, network, credentials, policy),
one gone "Setting no longer read: …" (notable), and of one in both a new `default` or `type`
(notable), a value gained or lost (lost: notable), a new `description` (a text diff), and its
deprecation (notable). Two builds whose options came from different kinds of source, or whose
variables were read differently, are not compared item by item: the change is where they were
read from. `source`'s tag is the build's and is no change. A table whose keys could not be read
in one build (its options there end at the table) is no evidence its keys were added or removed.

### desktop apps

`claude-desktop` (Claude Desktop, whose Code tab runs Claude Code through the Agent SDK) and
`codex-desktop` (the Codex app, which runs its threads in the `codex app-server` it bundles) are
versioned by the app's own release, its `CFBundleShortVersionString` (`4.16336.0`,
`26.1029.70936`), as the app's own update feed lists it: Claude Desktop's Squirrel
`RELEASES.json` (its current release alone), the Codex app's Sparkle appcast. Each release is
run whole (`test/e2e/profile_desktop_test.go`) on a hosted macOS runner, a fresh machine with a
desktop session, for one first turn against the fixture provider, its window driven as a person
drives it: so what the agent sends the model, and how the app starts it, are the app's own.
`capture.notes` say how each run differed from a person's at the app: how it was signed in
without an account (Claude Desktop in its third-party inference mode, set up as an
organization sets it up; the Codex app with an API key and the fixture as its model provider),
and how its window was driven (the accessibility API; the app's DevTools protocol).

A desktop app's snapshot has the agent's sections as the agent's own snapshot has them (`model`,
`prompt`, `tools`, `request`, `caching`, `side_requests`; `storage`, what the agent and the app's
record of its sessions wrote; `config`, the agent's, read from its executable or, for Codex, its
source at the build the app runs), and:

```json
"cli": {
  "help": { "--help": "…the agent's, as the app bundles it…" }, "commands": { },
  "launch": { "argv": ["<app-data>/claude-code/2.1.317/91184d3d9ba3/claude.app/Contents/MacOS/claude", "--output-format", "stream-json", "--model=claude-opus-5-5", "…"],
              "env": ["ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_ENTRYPOINT", "…"], "env_values": { "CLAUDE_CODE_ENTRYPOINT": "claude-desktop-3p", "ANTHROPIC_AUTH_TOKEN": "<redacted>" },
              "inherited": 12, "parents": ["<app>/Contents/Helpers/disclaimer --pgroup -- <app-data>/claude-code/2.1.317/…"] },
  "app_server": { "ClientRequest": ["thread/start", "turn/start", "…"], "ServerNotification": ["turn/completed", "…"] }
},
"binary": {
  "…": "the agent executable's facts, as for the agent's own snapshot", "signing": { "…": "the agent's signature" },
  "agent": { "harness": "claude", "version": "2.1.317", "path": "<agent>", "source": "downloaded",
             "download": "https://downloads.claude.ai/claude-code-releases/2.1.317/darwin-arm64/claude.app.tar.zst",
             "checksum": "…", "sdk": "@anthropic-ai/claude-agent-sdk 0.3.317" },
  "app": { "name": "Claude.app", "bundle_id": "com.anthropic.claudefordesktop", "version": "<version>", "build": "<version>",
           "download": "https://downloads.claude.ai/releases/darwin/universal/<version>/Claude-<hex>.zip", "size_bytes": 996455044,
           "signing": { "format": "developer-id", "team_id": "Q6L2SF6YDW", "notarized": true, "status": "valid", "…": "as binary.signing" } }
}
```

| field | |
|---|---|
| `cli.launch` | how the app started its agent: the command line and environment its process was started with, read from the kernel (`KERN_PROCARGS2`), and the processes between the app and it (`parents`, the agent's parent first). `env` are the variables the app set: those neither the app's own environment nor its user's login shell's has with the same value; `inherited` how many more it passed on as it had them. `env_values` are their values, a secret's `<redacted>` (by its name or its shape), a list of folders `<path>`, a login session's variable the app changed `<session>`, a key made for the run `<key>`, the machine's user and OS release `<user>` and `<os-release>` |
| `cli.app_server` | the JSON-RPC methods of the app-server the app drives, by the schema file of its `generate-json-schema` that names them: `ClientRequest`, `ClientNotification`, `ServerRequest`, `ServerNotification` |
| `binary.agent` | the agent the app runs: its harness and build, where the app runs it from, and how it comes by it: `bundled` in the app, or `downloaded` (Claude Desktop's, from `download` on first use, checked against `checksum`, the release's pin), and the SDK the app drives it with |
| `binary.app` | the app bundle: its name, bundle id, version and build, where it was downloaded from, its size, and its signature, read before it first ran (Developer ID, team, certificate, notarization, Gatekeeper's verdict) |

Placeholders of a desktop app's run, besides the agent's: `<app>` (the app bundle), `<app-dir>`
(the folder it was unpacked in), `<app-data>` (the app's own folder in Application Support),
`<agent>` (the agent's executable) and `<home>` (the runner's home, where the app and its agent
keep their state, as a person's do).

`support` is the capability checks of the build of the agent the release runs: the relay
scenarios run the agent build the app pins, launched as the app launches it
(`TestRelayClaudeDesktop`), and are recorded by that build's version, so they make no release of
their own and join each release that runs that build. Snapshots of `claude-desktop` and
`codex-desktop` published before the apps' own profiles are versioned by the agent build their
checks ran (`2.1.318`, `0.166.0`) and hold `support` alone.

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
      "id": "prompt.system.1.text",
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
| `id` | the change's `path` joined with dots: a list item by its key (`tools.Bash.description`, `support.capabilities.relay.telemetry@darwin/arm64.result`), by its position where it has none (`prompt.system.1.text`). Stable from diff to diff for the same thing, so a page may use it as an anchor, and a range's net change matches the timeline's steps by it |
| `category` | the snapshot section: `model`, `prompt`, `tools`, `caching`, `request`, `side_requests`, `behavior`, `telemetry`, `hooks`, `cli`, `config`, `network`, `storage`, `filesystem`, `binary`, `support` |
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

What the reference publishes of a changelog is bounded, so the pages that read it can rely on
the bounds rather than guard every input: a line is at most 2,000 bytes (a longer one is cut
there and ends `…`) and a version's text at most 128 KiB (cut at the last whole line that
fits). A note that was cut has `"truncated": true`. The largest published so far are 43.8 KB
of text (Codex) and a 758-character line (Pi), well inside both.

Lists are compared by key: tools by `name`, telemetry by surface then key, hooks by event (a
list of names, as `supported`, as a set), egress by host, method and path, capabilities by `id`
and platform. Strings of more than one line are compared as text.

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

## <harness>/lineage.json

Since which version the harness has each of its subcommands, command-line options,
configuration settings and environment variables, and until which: read from every published
snapshot of it, each time refgen renders. Nothing in it says when it was made, so a history
that did not change is a file that did not.

```json
{ "schema_version": 1, "harness": "codex",
  "window": { "oldest": "0.90.0", "newest": "0.166.0", "builds": 100 },
  "items": {
    "cli.commands.codex.options.--profile-v2": { "kind": "option",
      "ranges": [ { "since": "0.131.0", "until": "0.133.0", "removed": "0.134.0" } ], "since_bounded": true },
    "cli.commands.codex exec.options.--json": { "kind": "option",
      "ranges": [ { "since": "0.29.0", "until": null, "removed": null } ], "since_bounded": true,
      "source": { "tag": "rust-v0.29.0", "version": "0.29.0", "commit": "…", "url": "https://github.com/openai/codex/commit/…",
                  "file": "codex-rs/exec/src/cli.rs", "line": 61, "permalink": "https://github.com/openai/codex/blob/rust-v0.29.0/codex-rs/exec/src/cli.rs#L61",
                  "verified": "absent at rust-v0.28.0, present at rust-v0.29.0; … adds it",
                  "current": { "tag": "rust-v0.166.0", "version": "0.166.0", "file": "codex-rs/exec/src/cli.rs", "line": 88, "permalink": "…" } } } } }
```

`window` is the builds it was read from: the oldest and newest tracked and how many. Each item
is keyed by its path in a snapshot, joined with dots as a diff's change `id` is, so the two
join: `cli.commands.<command>` (`kind` `subcommand`: a command beneath the harness's own),
`cli.commands.<command>.options.<flag>` (`option`, by its first long name),
`config.options.<key>` (`setting`) and `config.env_vars.<NAME>` (`env_var`).

`ranges` are the spans of builds that have it, oldest first, more than one where it went and
came back: `since` the first build seen with it, `until` the last (null while the newest build
that observed it has it), `removed` the first build seen without it after `until` (null with
it). A build that did not observe the part an item would be in says nothing of it, and neither
ends a range nor starts one: a section not captured, a command whose own help was not read
(`helped` false: its options), a command the help that would list it was not read of, a
configuration read another way than the newest build's (`config.source` and `env_source`
without the tag), and what a minified reading misses where a diff takes it for not observed (a
nested key, a top-level one in `config.unobserved`, a table whose keys could not be read).
`since_bounded` is false where no build before the first range was seen without it, mostly
one in the oldest build tracked: its `since` is "that version or earlier". A nested setting, or
a subcommand's option, is no older than what holds it: first seen in the same build as it,
where that one's start is known, it is bounded too (builds that read settings only to the top
level say nothing of nested ones).

`source`, where the harness's source is public, is where an item already in the oldest build
came from: the earliest release tag (`tag`, `version`) whose source defines it, read the way
its snapshots are (its command line as clap's derive makes it from the structs and enums of
its source, its settings from the configuration source -source reads, its variables from the
names its code reads), the releases walked back from the oldest build until one does not; the
commit (`commit`, `url`) of those between that release and the one before that touch its
definition, the first whose tree defines it and whose parent's does not; and the line that
defines it there (`file`, `line`, `permalink`). `verified` says what was checked. `current` is
where the newest build tracked defines it, null where it is gone; a render moves it to a newer
build by looking in the file it was in first. With a source, the first range's `since` is its
release and `since_bounded` true. An item whose release or commit could not be told has none:
it is not guessed. Sources are looked up once, by hand (`refgen -lineage-sources`, below), and
every later render keeps them without the source; an item introduced within the window needs
none, as every release in it is tracked.

`refgen -lineage-sources` (Codex only so far) reads a local clone of the harness's repository,
kept between runs under `-source-clones` (else `$TERMA_SOURCE_CLONES`, else
`$XDG_CACHE_HOME/terma-harness-reference`, `~/.cache/terma-harness-reference`), as
`<owner>-<name>`: `openai-codex`. It clones it there the first time (blobless: a tree's files
are fetched the first time something reads them) and fetches its new tags after. Any render
that finds a clone there reads a release's source from it (for `-source` too) rather than its
tarball, and falls back to the network for a tag it does not have.

## Comparing any two versions

Diffs are published between consecutive versions only; any other two are compared where they
are read, by the same code that made the published ones (`refdiff`), compiled to WebAssembly:
`refdiff.wasm` with the Go runtime that built it, `wasm_exec.js`. After

```js
const go = new Go();
const { instance } = await WebAssembly.instantiateStreaming(fetch("refdiff.wasm"), go.importObject);
go.run(instance);
```

Its `changelog.text` joins the range's changelogs newest first, each under a `## <version>`
heading.

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

## recent.json

The newest versions across every harness, at most 200, newest release first (a version whose
release date is not known by when it was captured): each a version entry as its harness's
index has it, with `harness` and `name`.

```json
{ "schema_version": 1, "generated_at": "2026-11-04T05:02:11Z",
  "versions": [ { "harness": "codex", "name": "Codex CLI", "version": "0.166.0", "released_at": "…", "previous": "0.165.0", "changes": 4, "highlights": ["…"] } ] }
```

A capability tested on a platform for one build and not the other is no change: only a result
that changed is. `captured_at` and `capture.sections` move only when what was captured
changed: a night that captures a build the same as before leaves them.
