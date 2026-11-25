# Where these fixtures come from

`1.19.6/<case>/plugin-view.jsonl` is what a plugin saw in real OpenCode 1.19.6 sessions:
every event and hook call, recorded by a read-only probe plugin loaded beside terma's.
The replay tests in `terma_test.js` feed them to the plugin's OpenCode 1 entry.

`2.4.1/<case>/plugin-view.jsonl` and `2.x-scripted/<version>/<case>/plugin-view.jsonl` are
the same for OpenCode 2, fed to the plugin's OpenCode 2 entry. The OpenCode 2 section below
says how they were made.

- Captured 2026-11-25 with real OpenCode 1.19.6 (`opencode-ai`) and real OpenRouter calls
  on `anthropic/claude-haiku-5.5` (`07-patch` on `openai/gpt-5-nano`). `11-api-error` asks
  for a model that does not exist; `20-custom-provider-marked` goes through a custom
  OpenAI-compatible provider.
- The content is real model output about scratch files (`notes.txt`, `calc.py`) in made-up
  repositories.
- Scrubbed before they were copied here: the home path and scratch paths are
  `/home/user/…`, the login name is `user`, the host name is `dev-host`, and every key,
  bearer token, provider account id and email address outside `acme-qa.test` was replaced
  with a placeholder. `Authorization` and cookie headers were never recorded. Session,
  message and tool-call ids are kept.

## What `trim.mjs` removed

The scrubbed capture holds far more than the plugin reads. `trim.mjs` keeps:

- the `plugin.input` record of each plugin instance, reduced to its directory;
- the events the OpenCode 1 entry reads, plus `file.edited`;
- the hooks it registers, plus `tool.execute.after`; the `config` hook reduced to the
  config's top-level keys (the probe kept no MCP server names); `chat.params` reduced to
  the fields the entry reads.

It drops every other record (`message.part.delta`, `session.status`, `session.diff`,
`chat.headers` and the like), deletes each part's `metadata.openrouter` object (opaque
provider signatures) and empties reasoning text.

## Regenerating

From the scrubbed capture's `1.19.6` folder:

```bash
node testdata/trim.mjs <fixtures-scrubbed>/1.19.6
```

The script is deterministic. After regenerating, check that nothing under `testdata/`
holds a key shape, an email outside `acme-qa.test`, a real home path or a host name.

## OpenCode 2

Captured 2026-11-25 on Linux, with the same read-only probe plugin's OpenCode 2 form, which
also records each `setup` call.

- `2.4.1/<case>`: real OpenCode 2.4.1 (`@opencode/cli`) sessions with real OpenRouter calls,
  as the 1.19.6 ones, in the same cases where a case applies to OpenCode 2, plus
  `12-background-service` (one background service with three directories, so each event
  reaches the plugin two or three times), `13-tui-acp` (an ACP session) and
  `14-serve-compact-rename` (`opencode serve`, a manual compaction and a rename by hand). These
  were scrubbed as the 1.19.6 ones were.
- `2.x-scripted/<version>/<case>`: OpenCode 2.0.0 and 2.4.1 driven by a scripted
  OpenAI-compatible provider with no account (model `m`, provider `fake`), in scratch homes
  whose path is replaced by `/home/user`. They are the only OpenCode 2.0.0 sessions here. Each
  case: `sub` a subagent, then the child resumed alone in a new process; `retry` steps retried
  after a rate limit; `always` a step rate-limited until OpenCode gives up (the 2.4.1 run
  ended while it still retried); `err` a provider that refuses every request, twice, in two
  processes; `mcp` an MCP tool called by its flat name, which 2.4.1 repairs into code mode;
  `codemcp` MCP tools called from code mode; `compact` a manual compaction that fails; `rel`
  file and search tools with relative paths, run from a subdirectory; `rm` a session deleted;
  `attach` a prompt with a file attached by `opencode run -f`, whose content OpenCode puts
  into the prompt's text; `svc2` a background service with two prompts that attach a file
  through the API (as the TUI's `@file` does, one of them outside the repository), a shell
  command the person ran in the session, a patch that moves a file and one that adds and
  deletes;
  and, on 2.4.1 only, `svc` a background service with a prompt queued while a step ran, a
  session moved to another directory, a rename by hand and a compaction. They also hold
  `lookup` records: what `ctx.session.get({ sessionID })` really answered at that moment,
  which the tests use as OpenCode's answer.

### What `trim2.mjs` keeps

- each `setup` call, reduced to its directory and `app`;
- the events the OpenCode 2 entry reads, without their `durable` envelope, plus
  `session.shell.started` and `session.shell.ended`, which the tests use to prove a person's
  own shell command is no call;
- the hooks it registers (`session.prompt`, `session.title`, `session.compaction`,
  `session.generate`, `tool.execute.before`, `tool.execute.after`), plus `session.experimental.idle` and
  `session.retry`, which the tests use to prove they are ignored; each hook's `system`,
  `messages`, `tools`, `options` and `headers` are dropped;
- every `lookup` record, whole.

It drops every other record: deltas, `session.http.*`, `session.context`,
`session.model.request`, the `*.updated` events, and the probe's own notes.

### Regenerating

From the scrubbed capture's `2.4.1` folder and the folder of scripted sessions
(`<version>/<case>/plugin-view.jsonl`):

```bash
node testdata/trim2.mjs <fixtures-scrubbed>/2.4.1 <scripted>
```

The script is deterministic. Check the output as for the 1.19.6 fixtures.
