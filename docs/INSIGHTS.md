# Reading what your agents did

`terma usage`, `terma session` and `terma principal` read back what connected agents
exported: sessions, their events and git activity, who ran them, and what they cost.
This page is for a person — or an agent shelling out to `terma` — that needs the
numbers to mean what they appear to mean.

All three use the selected profile and project, with the same login or server key as
every other command (`TERMA_API_KEY` for headless use). Output follows `-o`; without it
a terminal gets a table and anything else gets JSON, so a script never has to ask.

## "How much did Dawson use today?"

```sh
terma usage --user dawson --since today
```

- `--user` takes a name, email, alias or id and resolves it through the principal
  catalog. One person is often several principals (the same email seen by Claude Code
  and by Codex); a name match returns all of them, so the answer covers every agent
  they use. A substring that lands on two different people is an error naming both —
  use the full email, the alias, or the id.
- `--since` / `--until` take RFC 3339, a date (`2026-09-09`), a relative age (`90s`,
  `15m`, `2h`, `7d`), `today`, `yesterday` or `now`. The window is half-open,
  `[since, until)`. Default: the last 24 hours. `today` and `yesterday` are local
  midnights; the JSON echoes the resolved bounds in UTC.
- `--group-by user | api-key | source | model | provider | none` (default `user`).
  Principal groups carry `source_system`, because Claude Code's user ids and Codex's
  are different namespaces.
- `--source`, `--model`, `--provider`, `--api-key` narrow the slice; repeat a flag for
  any-of.

```json
{
  "basis": "metrics_window",
  "since": "2026-09-09T04:00:00Z",
  "until": "2026-09-09T15:12:03Z",
  "group_by": "user",
  "filters": { "user_id": ["7c5c8f…"] },
  "rows": [
    {
      "group": { "source_system": "claude-code", "user_id": "7c5c8f…" },
      "name": "Dawson",
      "cost_usd": 31.53,
      "input_tokens": 22410922,
      "output_tokens": 250606,
      "cache_read_tokens": 21628862,
      "cache_write_tokens": 732987,
      "total_tokens": 45023377,
      "model_calls": 129
    }
  ],
  "totals": { "cost_usd": 31.53, "total_tokens": 45023377, "…": "…" },
  "queries": ["sum by (source_system, user_id) (increase({__name__=\"terma.ai.cost.usd.total\", …}[40323s]))", "…"]
}
```

### What the numbers are

`usage` reads the platform's `terma.ai.*` counters — cost, the four token buckets,
and model calls — which the platform derives from every model call that **settled**.
`basis: metrics_window` means: spend that happened inside the window, whichever
session it belonged to. A session that started yesterday and ran past midnight
contributes today's portion to today.

The counters are sampled over time and read with PromQL `increase()` (the same query
the web app's insights page runs; the JSON carries the exact expressions under
`queries`). Two consequences:

- The window's edges are interpolated to the nearest samples, so a figure can differ
  from a hand-sum of session roll-ups by a fraction of a sample interval. Token and
  call counts are rounded to whole numbers; cost is left as read.
- A model call that has not settled yet is not counted until it does.

`cost_usd` is the provider's price for those calls. Cache tokens are reported
separately because providers bill them at different rates; `total_tokens` is the sum
of all four buckets.

## The sessions behind the numbers

```sh
terma session list --user dawson --since yesterday --until today
terma session list --source codex --model gpt-5 --all
terma session list --provider anthropic --sort cost --page-size 20
terma session list --filter 'api_key_id="30692719"'
terma session list --page 2
terma session list --follow
```

`session list` selects by when a session was **last active** (its latest event), not
by when it started: a session that began last week and ran this morning is inside
`--since today`. The two bounds are not applied in the same place:

- `--since` is sent to the server (`active_after`), as an absolute instant.
- `--until` is applied by `terma`, to the same field (`last_activity_at`, exclusive).
  The server has no upper bound to give it to. So that a page is not silently short,
  `terma` keeps reading server pages until `--page-size` sessions have passed the
  bound (every page, under `--all`). It cannot be combined with `--page`: a page number
  names one of the server's pages, and the filtered rows do not line up with them. A
  session that reports no `last_activity_at` is left out.

Sessions are ranked most recently active first; `--sort recency | cost | tokens |
turns | tools` ranks them otherwise, always highest first. One page of 100 by default:
`--page-size` sets the size (1–1000), `--page` asks for a later page (counting from
1), and `--all` follows every page. `--source`, `--user`, `--api-key`, `--model` and
`--provider` narrow the slice; repeat a flag for any-of. `--filter` takes a raw AIP-160
expression over `source_system`, `user_id`, `api_key_id`, `model` and `provider` for
anything the flags do not express; it is ANDed with them.

Each session is a roll-up over its whole life: turns, model and tool calls, token
usage and cost, models and providers used, and git counters (commits, pushes, pull
requests, files touched) with the `repositories` and `git_orgs` they touched. Cost is
`usage.provider_cost_usd`, in dollars. JSON rows carry `user_name` / `api_key_name`
alongside the ids so a consumer never needs a second call to label them.

A single page carries the server's `pagination` — `page`, `per_page`, an exact `total`
and `total_pages` — so the next page is `--page <page + 1>` while `page < total_pages`.
A walk (`--all`, or any `--until`) gathers rows across pages and reports no
`pagination`. Paging is by offset over a list that moves as sessions become active:
`--all` delivers a session once even if it turns up on two pages, and ends with an
error saying the results are incomplete rather than looping if the server's page
counts stop making sense.

Because a session's usage is lifetime usage, summing `session list` over a day and
`terma usage --since today` legitimately disagree whenever a session straddles the
window's edge. Use `usage` for "how much in this period"; use `session list` for "which
conversations, and what did each cost".

### One session

A session is identified by its **session id together with its source system**. The
session id is the opaque `session_id` on a `session list` row; it means nothing outside
the project it came from. Copy both from `session list`.

```sh
terma session get    <session-id> --source claude-code
terma session events <session-id> --source claude-code
terma session events <session-id> --source claude-code --tools-only
terma session events <session-id> --source claude-code --errors-only
terma session events <session-id> --source claude-code --kind model_call --kind compaction
terma session events <session-id> --source claude-code --since 2026-09-09T09:00:00Z --until 2026-09-09T10:00:00Z
terma session git    <session-id> --source claude-code
terma session git    <session-id> --source claude-code --follow
```

`get` prints the same roll-up a list row carries. The server offers one session's
roll-up only as a live feed, so `get` opens it, takes the first summary and hangs up;
if none arrives within 15 seconds it fails with `the gateway sent no summary of the
session within 15s` rather than waiting.

`events` replays the conversation oldest first: `session_start`, `turn_start`,
`user_message`, `assistant_message`, `model_call` (with usage once settled),
`tool_call`, `tool_result`, `tool_decision`, `compaction`, `error`. Content is whatever
the harness was connected to export — a harness connected with `--exclude-prompts` or
`--exclude-tool-content` carries none of that. The table folds each event's content to
one line of `--content-width` characters (default 80; `0` for all); JSON is verbatim.
The history is read to the end however many pages it takes, so the output is the whole
session (or the whole `--since`/`--until` window, a half-open bound on event time that
the server applies). Each JSON event carries `logical_event_id`, its stable identity,
with a `version` that rises when the server corrects it, and its own `cursor`.

`git` lists the branches, commits, pushes and pull requests an agent's tool calls
produced, as evidence of actions — not a deduplicated commit ledger. Each activity
carries the join keys back to a session event (`tool_call_id`) and to GitHub
(`repository_key` plus a sha or PR URL).

### Live tails

`session list --follow` and `session git --follow` stream. As a table, each update is
one line; as JSON, each is one newline-delimited envelope. The two feeds are shaped
differently.

`session list --follow` is not a change feed. The server sends the **whole first
page** of the list — the same `--since`, `--sort`, `--page-size` and filters, never a
later page, so `--page`, `--all` and `--until` are refused — and sends it again on a
timer whether or not anything changed:

```json
{"event":"snapshot","data":{"sessions":[{…},{…}],"pagination":{…}}}
```

Replace your copy with each `snapshot`; do not merge. `terma` drops a snapshot that is
byte-identical to the one before it, so every line is a change. As a table it prints one
line per session that is new or differs from the row last printed for it.

`session git --follow` is an upsert feed:

```json
{"event":"upsert","data":{"activity":{…}}}
{"event":"snapshot_completed","data":{}}
{"event":"upsert","data":{"activity":{…}}}
```

Key git activities by `activity_id`, replacing on each `upsert`. `snapshot_completed`
marks the end of the initial state, not the end of the stream.

The server rotates either connection after an hour and `terma` exits with an error
saying so — rerun to reconnect; a reconnect always starts with a fresh snapshot.

## Who is behind an id

```sh
terma principal list
terma principal list --kind user --source claude-code
terma principal find dawson
```

Sessions and metrics carry ids, never names. The catalog maps each id to the name the
provider reported (a user's email, an API key's label) and any alias set in the web
app; `find` matches the way `--user` does. Setting or clearing an alias is done in the
web app — the API these commands use is read-only.

## Which session produced a commit

```sh
terma blame                 # HEAD
terma blame <commit>        # any revision git understands
terma blame <commit> -o json
```

`blame` is the reverse of `session`: given a commit, it names the agent session that
produced it. It reads the commit locally to get its sha and time, then reads back the
`terma.commit` record the post-commit hook exported for that sha — the stamped
`Agent-Session-Id`, the tool, the line counts, and the repository.

- The commit must be in the current repository; the revision is anything `git show`
  accepts (`HEAD`, `HEAD~3`, a sha, a tag).
- The lookup is a single log query windowed tightly (±1h) on the commit's own time, so
  a commit of any age resolves without scanning back from now.
- JSON carries `session_id` and the full `sessions` list (a commit can be stamped for
  more than one session), `tool`, `source_system` (the tool without its version),
  `lines_added` / `lines_deleted` / `file_count`, `branch`, `repo_url` and
  `reported_at` (when the backend recorded the commit).
- Per-commit **cost** is not reported yet. The session id here is the harness session
  id, which the usage metrics do not key on; joining a commit to its spend needs the
  AI-session lookup and lands in a follow-up. Attribution works for every harness that
  stamps commits. Cursor also captures ordered hook observations and optional token
  snapshots, but backend usage and billing mapping remain pending; its attributed
  sessions do not yet have a cost in these commands.

## Errors worth knowing

| You see | It means |
|---|---|
| `"da" matches several principals (Dana, dawson@mirador.org)` | Be more specific; nothing was queried. |
| `no claude-code session "…" in this project` | Wrong session id, wrong `--source`, or wrong project. |
| `the gateway sent no summary of the session within 15s` | `session get` reached the server but it never answered with the roll-up; rerun. |
| `session listing … results are incomplete` | `--all` (or `--until`) stopped because the server's page counts stopped making sense; what was printed is not the whole list. |
| `--until is applied after the server pages the list …` | `--until` is client-side, so it cannot be combined with `--page`; use `--all` or a larger `--page-size`. |
| `--since (…) must be before --until (…)` | The window is inverted or empty. |
| `INVALID_FILTER` from the gateway | A `--filter` expression the gateway's grammar rejects — its message names the rule. |
| `the server closed the stream (connections rotate hourly)` | Normal for `--follow`; rerun. |
| `No usage recorded for this window and filter.` | A valid answer, not a failure: nothing settled in that slice for that selection. |
| `<sha> carries no Agent-Session-Id trailer` | A human commit, or one made without terma — nothing to attribute. |
| `<sha> is stamped … but has not reached Terma yet` | The trailer is there but its `terma.commit` event has not been delivered; run `terma spool flush`. |
