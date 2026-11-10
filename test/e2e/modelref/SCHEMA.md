# Model Reference: published data, schema version 1

What each AI model a coding agent runs on is and does: what its vendor says of it, what its
vendor's API does with the parameters an agent sends, which harnesses use it, and what changed
since the model before it and since the reference last looked. `modelref` reads every source,
merges what it read into the published tree, and the reference workflow
(`.github/workflows/models.yml`) publishes the tree beside the Harness Reference on the
`compat-matrix` branch, under `docs/models/`. Everything here is public:
https://raw.githubusercontent.com/miradorlabs/terma-cli/compat-matrix/docs/models/index.json

Every file carries `"schema_version": 1`. A field may be added within a version; nothing is
renamed or removed without bumping it. Absent and `null` mean "not known", never "none" or zero:
an empty list or object means known and empty. A file changes only when what it says changes: a
run that finds nothing new writes the same bytes.

## Vendors and sources

The reference follows Anthropic (`anthropic`), OpenAI (`openai`), Google's Gemini API (`google`)
and xAI (`xai`). It follows a model its vendor lists: a model in the vendor's own docs (Anthropic's
model status table; OpenAI's, Google's and xAI's model indexes) or, where a key is configured, its
API's model list (Anthropic's, xAI's). For OpenAI and Google, it follows a model that reads and
writes text (not an embedding, image, voice, live or moderation model).

| source           | what it is                                                                   |
| ---------------- | ---------------------------------------------------------------------------- |
| `<vendor>-api`   | the vendor's model list endpoint (`GET /v1/models` and kin), with its key     |
| `<vendor>-docs`  | the vendor's documentation, read as Markdown: its index and each model's page |
| `models.dev`     | https://models.dev, an open catalog kept by its community                     |
| `openrouter`     | OpenRouter's model list, https://openrouter.ai/api/v1/models                  |

A fact takes the value of the first source in that order that states it. A source not read on a
run (an outage, no key) keeps what it said before: a vendor's outage never reads as its models
going away, and a model is "unlisted" only when a vendor source that lists models was read and did
not list it.

## Layout

```text
docs/models/index.json                 every vendor, every model, newest release first
docs/models/recent.json                the newest events across every model, for a feed
docs/models/<model>/snapshot.json      everything the reference holds of a model, as last observed
docs/models/<model>/notes.json         its release notes: what's new, against the model before
docs/models/<model>/timeline.json      what changed in what the reference observes, newest first
```

`<model>` is the vendor's API id (`claude-fable-5-2`, `gpt-6.1-sol`, `gemini-4-argon`, `grok-4.7`):
a letter or digit, then letters, digits and `.`, `_`, `-`. Ids are unique across the four vendors.
The site's pages are `/models` and `/models/<model>`, and `terma.ai/m/<model>` is the short link.

## index.json

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-10T06:45:00Z",
  "vendors": [
    { "id": "anthropic", "name": "Anthropic", "docs": "https://platform.claude.com/docs/en/about-claude/models/overview",
      "changelog": "https://platform.claude.com/docs/en/release-notes/api", "models": 25 }
  ],
  "models": [
    {
      "id": "claude-fable-5-2", "vendor": "anthropic", "name": "Claude Fable 5.2", "family": "claude-fable",
      "status": "active", "released_at": "2026-11-09", "first_seen": "2026-11-10T06:45:00Z",
      "predecessor": "claude-fable-5-1", "successor": null,
      "context_window": 1000000, "max_output": 128000, "knowledge_cutoff": "2026-06", "reasoning": true,
      "harnesses": ["claude"], "last_change": { "at": "2026-11-10T06:45:00Z", "changes": 3 }, "probed": true
    }
  ]
}
```

`generated_at` is the time of the newest change the reference holds. `status` is the vendor's
(`active`, `deprecated`, `retired`; Anthropic's alone for now), or `unlisted` where the vendor no
longer lists the model; null where unknown. `released_at` and `knowledge_cutoff` are `YYYY-MM-DD`,
or `YYYY-MM` where the source names no day. `first_seen` is when the reference first listed it.
`family` is the model's line, its id without versions, dates and release qualifiers
(`claude-fable`, `gpt-sol`, `gemini-flash-lite`). `predecessor` is the model before it in its family
(the next lower version, the same vendor's), `successor` the one after. `harnesses` are the Harness
Reference's ids of the harnesses whose newest captured build uses the model in some role.
`last_change` is the newest timeline step with changes, null for none. `probed` says the reference
has probed its API (a model its key may not use is not probed).

## <model>/snapshot.json

```json
{
  "schema_version": 1,
  "id": "claude-fable-5-2", "vendor": "anthropic", "name": "Claude Fable 5.2", "family": "claude-fable",
  "first_seen": "2026-11-10T06:45:00Z",
  "listed": true,
  "facts": { "context_window": 1000000, "max_output": 128000, "reasoning_efforts": ["low", "medium", "high", "xhigh", "max"] },
  "provenance": { "context_window": { "anthropic-docs": 1000000, "models.dev": 1000000, "openrouter": 1000000 } },
  "disagreements": [],
  "sources": { "anthropic-docs": "https://platform.claude.com/docs/en/models/fable-5-2/overview", "models.dev": "https://models.dev" },
  "docs": {
    "overview":  { "title": "Model page", "url": "https://platform.claude.com/docs/en/models/fable-5-2/overview", "text": "…" },
    "whats_new": { "title": "What's new", "url": "https://platform.claude.com/docs/en/models/fable-5-2/whats-new-fable-5-2", "text": "…" }
  },
  "api": { "listing": { "…": "the vendor API's own object" }, "probes": { "…": "below" } },
  "harnesses": [
    { "harness": "claude", "name": "Claude Code", "role": "default", "first": "2.1.323", "last": "2.1.330", "builds": 8,
      "current": true, "params": { "output_config": { "effort": "medium" } } }
  ]
}
```

`facts`, each where some source states it:

| fact | value |
| --- | --- |
| `name`, `description` | the vendor's name and one-line description |
| `status`, `deprecated_at`, `retires_at`, `retires_not_before` | lifecycle, as the vendor states it |
| `released_at`, `knowledge_cutoff`, `training_cutoff` | dates |
| `context_window`, `max_input`, `max_output` | tokens |
| `input_modalities`, `output_modalities` | lowercase words (`text`, `image`, `pdf`, `audio`, `video`) |
| `reasoning`, `tool_calling`, `structured_outputs`, `sampling_parameters`, `open_weights`, `batch` | booleans (`sampling_parameters`: takes temperature and kin) |
| `thinking`, `latency` | the vendor's words (`adaptive (always on)`, `moderate`) |
| `reasoning_efforts`, `default_effort` | the effort levels it takes, and its default |
| `features`, `unsupported_features`, `tools`, `endpoints`, `regions` | lists, in the vendor's words |
| `aliases`, `snapshots` | other ids that name it |
| `platform_ids` | its id on each cloud (`Amazon Bedrock`: `anthropic.claude-fable-5-2`) |

`provenance` is each source's own value of each fact. `disagreements` names the facts whose
sources state different values (lists aside, which catalogs word each their own way; numbers within
a vendor's rounding, 1M against 1,048,576, agree; a date agrees with itself to a coarser precision).
`docs` are the vendor's pages about the model, whole, as Markdown: `overview` its model page,
`whats_new` its "what's new" page (Anthropic, Google) or getting-started page (xAI). `listed` is
false for a model its vendor no longer lists. `harnesses` are the ways the Harness Reference's
builds use the model: `role` `default` (the model a session starts on; `params` the request
parameters the newest such build sends with it), `alias` (`label` the alias), `subagents`,
`compaction`, `fallback`, `side_request`; `first` and `last` the oldest and newest tracked builds
that do, `current` whether the harness's newest captured build still does.

### api.probes

Where the vendor's key is configured, the reference sends the model small requests (a short
prompt, at most 16 output tokens), each the plain request changed in one parameter, and records
what the API did with each:

```json
{
  "at": "2026-11-10T06:45:00Z",
  "endpoint": "POST /v1/messages (anthropic-version 2026-10-21)",
  "params": {
    "baseline": { "accepted": true, "status": 200 },
    "sampling.temperature": { "accepted": false, "status": 400, "error": "temperature is not supported for this model." },
    "effort.max": { "accepted": true, "status": 200 }
  },
  "tokens": { "prose": 72, "code": 101, "json": 98, "cjk": 61, "symbols": 70, "indented": 52 },
  "tokenizer_like": ["claude-opus-5-5", "claude-sonnet-5-5"],
  "response": { "model": "claude-fable-5-2", "stop_reason": "max_tokens", "usage_keys": ["input_tokens", "output_tokens"],
                "headers": ["anthropic-ratelimit-requests-limit", "request-id"], "stream_events": ["message_start", "…"] }
}
```

`at` is when the probes first saw what they show: a run that sees the same keeps it. Where the API refused
the plain request (a model the reference's key may not use, Mythos models), `unavailable` says
why (`"404 model: claude-mythos-5-2"`) and no case was sent: `params` is empty. `params` are
the cases by name (`modelref/probe_vendors.go`: sampling, stop sequences, thinking and effort,
forced tool use, prefill, structured output, the output limit plus one, service tiers), each
`accepted` or not with the API's status and its error message (request ids taken out). A case the
API never answered meaningfully (the network, a 429, a 5xx) is left out, not counted as a refusal.
`tokens` are the token counts of a fixed corpus (`modelref/probecorpus.go`); `tokenizer_like` the
same vendor's models that count every text the same. `response` is a plain answer's shape: the
model the API says answered, why it stopped, its usage keys, the response headers a caller reads
(names only), the event kinds its stream sent; `fingerprint` where the vendor sends one (xAI).

## <model>/notes.json

```json
{
  "schema_version": 1,
  "id": "claude-fable-5-2",
  "predecessor": "claude-fable-5-1",
  "whats_new": { "title": "What's new", "url": "…", "text": "…" },
  "vendor_highlights": ["Editing earlier turns invalidates thinking blocks on every account", "…"],
  "changelog": [ { "date": "2026-11-09", "url": "https://platform.claude.com/docs/en/release-notes/api", "text": "* We’ve launched **Claude Fable 5.2** …" } ],
  "changes": [ { "id": "facts/default_effort", "category": "facts", "op": "changed", "path": ["facts", "default_effort"],
                 "title": "Default effort: high → medium", "impact": "notable", "before": "high", "after": "medium" } ],
  "highlights": ["Default effort: high → medium", "Accepts effort.max (Claude Opus 5 rejects it)"]
}
```

`vendor_highlights` are what the vendor says is new, in its own words: the headings (or the bold
leads of the list items) of its what's new page's sections on what is new or breaking, or else
the first sentence of each changelog entry that announced the model. `changelog` are the vendor's
changelog entries that name the model, by id or name, newest first, each a list item (or a
section) under its date. `changes` are what differs from the predecessor in what the model is and
what its API takes (facts, minus names, dates and ids; probes' accepted/rejected), each a change as
in the Harness Reference's diffs (`category`, `op`, `path`, `title`, `impact`, `before`, `after`). A
fact known for one of the two and not the other is not a change. `highlights` are the changes most
worth a headline, with the tokenizer's difference first where the corpus counts more than 1%
apart ("Counts 12% more tokens than Claude Fable 5.1 for the same text").

## <model>/timeline.json

```json
{
  "schema_version": 1,
  "id": "claude-fable-5-2",
  "steps": [
    { "at": "2026-11-12T09:15:00Z", "event": "changed", "changes": [ { "category": "api", "title": "API now rejects sampling.temperature", "impact": "breaking", "…": "…" } ] },
    { "at": "2026-11-10T06:45:00Z", "event": "listed", "changes": [] }
  ]
}
```

A step is a run that saw the model differ from the run before: `listed` (first seen, or listed again),
`changed`, `unlisted`. Its `changes` compare, run to run, the model's facts, whether it is listed, its
API listing and probes, the harnesses that use it (`harnesses/<harness>/<role>[/<label>]`, and whether
each still does), and its pages' text (a unified diff in `text_diff`). `category` is the section
(`listed`, `facts`, `api`, `harnesses`, `docs`); `impact` `breaking` (the API refuses a case it took, a
limit went down), `notable` (any other fact, probe or harness change), `informational` (a page's text,
the API listing's details).

## recent.json

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-12T09:15:00Z",
  "events": [ { "at": "2026-11-12T09:15:00Z", "model": "claude-fable-5-2", "vendor": "anthropic", "name": "Claude Fable 5.2",
                "event": "changed", "changes": 1, "highlights": ["API now rejects sampling.temperature"] } ]
}
```

The newest 200 timeline steps across every model, newest first.

## Announcements

`modelref -announce <file>` writes the models listed for the first time on the run, where their
vendor had models published before (or released on or after `-announce-since`), each with a post of
at most 280 characters (`text`, a link counted as X counts one) and a Slack message (`slack`) with a
button to `https://terma.ai/m/<model>`. The workflow posts the Slack messages; `text` is for X.
