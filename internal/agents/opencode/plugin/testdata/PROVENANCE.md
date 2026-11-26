# Where these fixtures come from

`1.19.6/<case>/plugin-view.jsonl` is what a plugin saw in real OpenCode 1.19.6 sessions:
every event and hook call, recorded by a read-only probe plugin loaded beside terma's.
The replay tests in `terma_test.js` feed them to the plugin's OpenCode 1 entry.

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
