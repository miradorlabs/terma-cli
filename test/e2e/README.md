# e2e

Real harnesses and the real `terma` binary, with credential-free fixture scenarios
and separate authenticated provider scenarios. Each scenario names the capability it
proves (`compat.go`), and the generated compatibility matrix is built from their results.

```sh
make test                      # offline value contracts; no harnesses or credentials
cp .env.example .env.local     # fill in what you have
make telemetry                 # real harnesses, local provider fixtures, no credentials
make run                      # build in Docker, run natively, write report/latest.md
make run RUN=TestClaudeSubscriptionSession
E2E_UPDATE_GOLDEN=1 make run # re-record the attribute key sets after a harness upgrade
```

Every scenario runs in a sandbox: scratch Terma config, scratch harness config, a
scratch git global config and a scratch git repository, with the machine-wide hooks
`terma setup` writes for a developer (the agents' user-level hooks), the commit hooks a
claimed session installs in the scratch repository's own `.git/hooks`, and the export
pointed at an OTLP receiver inside the test:
through the local relay, or straight there by an exporter the sandbox writes into the
agent's own config. The team policy's repository list (`WithRepositories`, default the
scratch repository's origin, `github.com/acme/repo`) decides where the hooks record
anything. Contracts then read
three planes together:

- the hook events (`terma.session.start`, `terma.session.quota`, `terma.files.touched`, `terma.commit.stamped`, …), read from the spool file or, more often, from what the end-of-turn flush has already delivered to the receiver,
- the receiver (the harness's own OTLP records: `api_request` with `speed`, `cost_usd`, `session.id`, resource identity; and Terma's own delivered events, as `service.name=terma-cli` logs stamped with the project, which is the shape the backend parses),
- the terminal (the pseudo-terminal the interactive harness draws into: replies, Terma's mark, the pre-existing status line).

Delivery is asserted, not assumed: the profile's ingest URL is the receiver, the
`Stop` and `SessionEnd` hooks start the background flushes a developer's machine
would start, and the contracts wait for the events to arrive.

Interactive sessions are driven through a real pty because the status line, the
trust dialog and permission prompts only exist there. A terminal emulator
reconstructs incremental redraws when matching replies.

Value contracts check finite, nonnegative token counts and positive completed-turn
usage and Claude cost, while allowing zero cache counts and empty Codex response
records preceding the reply. Claude's separate input
buckets are summed; Codex's cached input must fit inside its total input. Quota
percentages must be finite and nonnegative (Claude can report more than 100%),
and resets must be after the observation timestamp,
so delayed delivery does not turn a valid snapshot into a failure. Claude's quota
prompt ID must join to a request in the same session; it need not be the first
request received. Codex's subscription windows also need positive durations.

`make test` exercises these checks with synthetic valid and malformed evidence,
including timestamp preservation through OTLP delivery. It skips real harness
scenarios explicitly. A passing offline run does not establish provider
compatibility; `make run` runs those scenarios and propagates a failing test's
exit status to the caller.

The receiver retains complete OTLP payloads; telemetry tests validate
logs, traces and metric values together.

## Credentials and modes

| variable | enables | route |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | isolated Claude Code sessions on a subscription | subscription |
| `TERMA_E2E_REAL_LOGIN=1` | the same with the developer's own login (no token needed) | subscription |
| `ANTHROPIC_API_KEY` | headless Claude Code on the Console route | api_key |
| `OPENAI_API_KEY` | Codex on the API route | api_key |

A missing credential makes its scenarios "not run" in the report. Nothing here
passes by absence.

`TERMA_ENV` is the one terma setting the sandboxes inherit (`TERMA_ENV=dev make run …`),
so anything that is not the in-test receiver stays off production. How the sandbox
is set up without an account is under "Sandbox setup" at the end.

For Codex API tests, the suite pipes the key to `codex login --with-api-key` in
its scratch `CODEX_HOME`, using `cli_auth_credentials_store="file"` for login and
execution. Supplying `OPENAI_API_KEY` alone left requests unauthenticated on the
tested builds. The temporary profile is removed after the test; the key is never
put in command arguments. See [Codex authentication](https://learn.chatgpt.com/docs/auth).

Both authentic API routes passed on 2026-09-15: Claude 2.1.270–2.1.272 and Codex
0.153.3–0.154.0. Run only these scenarios with:

```sh
TERMA_E2E_REPORT=report/api-routes make run RUN='^(TestClaudeAPIKeyHeadless|TestCodexAPIKey)$'
```

`TestClaudeIsolatedRenderer` and `TestClaudeStopFailureDelivery` use real Claude
binaries with controlled loopback responses and dummy keys. They verify installed
hook/renderer behavior independently of provider credentials; they do not verify
the authentic API billing route.

## CI

The main CI workflow runs this module's offline contracts with the race detector
and the credential-free telemetry scenarios against pinned real harness builds,
tolerating only Codex's documented SessionEnd race there
(`TERMA_E2E_KNOWN_UPSTREAM=codex-session-end`; see `TestCodexSessionEndProbe`).
`E2E harness contracts` (`live.yml`) runs nightly and on manual dispatch against the latest
three releases. Claude uses GitHub OIDC with Anthropic workload identity
federation; no Anthropic API-key secret is needed. The workflow contains the
non-secret federation, organization, service-account and workspace IDs. Each
Claude process gets a fresh identity token in a private temporary file and
exchanges it through Claude Code's native federation support. These short
headless scenarios use API billing, not subscription credentials.

The workflow file `live.yml`, its `live-harnesses` environment and CI's `live-contracts`
check keep those names: the federation rule below, the OpenAI secret and the repository
ruleset name them.

The Anthropic rule is configured for inference only (`workspace:inference`) in
workspace `terma-cli-ci` (`wrkspc_01Ug36g2VPKLUSFZ9LZXFTvU`), with a maximum token
lifetime of 600 seconds. Its match requires all of:

- Audience `https://api.anthropic.com`.
- Subject `repo:miradorlabs@243301318/terma-cli@1383906316:environment:live-harnesses`.
- Repository `miradorlabs/terma-cli`, repository ID `1383906316`, owner ID `243301318`.
- Ref `refs/heads/main` and workflow
  `miradorlabs/terma-cli/.github/workflows/live.yml@refs/heads/main`.
- Event `schedule` or `workflow_dispatch`.

Manual dispatches must target `main`. Branch-only subjects, pull requests, pushes,
and other workflows cannot use this rule. The service account has the developer
organization role; the rule narrows its minted tokens to inference in this workspace.
Workspace spending caps and rate overrides must be configured in the Console;
they have not been set by this provisioning step.

Codex still requires `OPENAI_API_KEY`, a restricted service-account key in a
dedicated CI project, under GitHub Settings → Environments → `live-harnesses`.
OpenAI provisioning uses project `terma-cli-ci` (`proj_lQLLGXqCoNP9vAVYyhJokAu5`)
and service account `terma-cli-live-ci`. Its GitHub secret contains a scoped API
key with `api.responses.write` and `api.model.read`; the initial unrestricted
key was deleted. A direct Responses request and model listing both succeeded.
A $10/month project spend limit was configured, but OpenAI returned enforcement
status `inactive`; do not rely on it as an enforced cap until that is resolved.

Local Claude API-key runs remain supported with `ANTHROPIC_API_KEY`; configured
federation takes precedence in the real API scenario. Synthetic provider tests
continue to use only dummy credentials.

Missing API credentials fail the preflight. Subscription scenarios receive no
subscription credentials on nightly runs and are reported as **not run**, not
passed. The GitHub job summary includes the detailed coverage report.

To run subscription checks manually, enable **include_subscriptions** when
running the workflow and supply `CLAUDE_CODE_OAUTH_TOKEN` and `CODEX_AUTH_JSON`
(a dedicated test account's auth.json) in the same environment. Both are then
required. Refresh the Codex login when it expires; it is copied into a private
temporary file and removed after the job. Only summary reports are retained.

### Provisioning CI credentials

Create dedicated credentials in each provider console; do not reuse a personal
login, production key, or admin key. For Anthropic, restrict the service account to the CI
workspace and set a low monthly spend cap and conservative rate limits there.
For OpenAI, create a CI project and service account, restrict inference permissions
to what Codex needs, and configure model access/rate limits. Verify the allowed
model against the harness's default model before running the suite. Project budget
alerts should not be treated as a hard spending cap.

Store each key using the interactive secret prompt (never put its value in a
command argument, source file, or chat):

```sh
gh secret set OPENAI_API_KEY --repo miradorlabs/terma-cli --env live-harnesses
```

After this workflow change is on the branch GitHub will run, trigger and inspect
an API-only run:

```sh
gh workflow run live.yml --repo miradorlabs/terma-cli -f include_subscriptions=false
gh run list --repo miradorlabs/terma-cli --workflow live.yml --limit 3
```

The OpenAI API key is supplied only to credential preflight and the test run, not
checkout or build steps. An API-only pass does not establish subscription-route
compatibility.

## Cost

Claude uses Haiku with short prompts and a budget flag for headless runs; the
multi-turn scenario intentionally sends two prompts. Codex uses the tested build's
default model with low reasoning effort. Short replies can still involve sizable
input context. The report sums available harness-reported USD; Codex usage is
recorded in tokens and is not priced there. Treat that sum as partial, not the
combined provider bill.

## Golden key sets

`golden/<harness>/<surface>.json` holds the attribute names each surface
carried when last recorded. A missing key fails (interface drift); a new key is
noted in the report so the matrix can grow. Re-record with `E2E_UPDATE_GOLDEN=1`
after checking what changed.

## Field census and drift

The goldens pin the few surfaces terma parses. The field census covers everything else:
every attribute key each harness build exports over OTLP, on every surface the relay applies
its content policy to (a log event, a span, its events and links, a metric and its exemplars,
the instrumentation scope, the resource), with the kinds of value it carried (`text`,
`number`, `bool`, `list`, `map`, `bytes`). The workload scenarios' direct runs and
`TestClaudeInteractiveFields` (the events only an interactive session sends, such as
`permission_mode_changed`) record it, and the run writes `report/fields.json`, and beside it
`report/census.json`: the builds those scenarios ran (`TakesCensus`), and whether one failed
for a build, whose census is then partial. Each key is
classified by the terma under test, `terma relay classify`, as its relay treats it where it
sits (a record, a resource or a span event, as the census saw it) when a project withholds
content; the relay also names the kinds of value it keeps of the key there, so the catalog
and the digest judge a key withheld from what the relay says, never from a rule of their own:

| class | what the relay does |
|---|---|
| `safe` | sends it, whatever the policy |
| `prompt` / `tool_content` | sends it only when the project collects prompts / tool content |
| `unclassified` | drops it, and counts it, unless it keeps the value's kind: a number or flag on a record, nothing on a resource |

The catalog, `docs/compat/fields.json`, lives beside the compatibility history on the
`compat-matrix` branch, and `docs/FIELDS.md` renders it: per harness, per surface, each key's
class, kinds, the first build it was seen in, and whether the newest build censused whole
still has it (a partial census's absences are no evidence). A
key keeps its first build and its newest five, and its kinds and class are the newest
build's, and apart from them the newest builds a whole census saw it in, the evidence a
build sent it (what only a failed run saw, an error path's, is not, though the build is
censused whole another night); a harness keeps its newest
five censuses, and five whole ones however old (a
partial census, one a census scenario failed before taking whole, does not push out the
whole ones), and a key leaves once none of them saw it. The file holds one entry a line, so a night's change is a diff of what changed.

Every night `live.yml`'s `compat` job starts from the published history and catalog, says
what the night changed against them, then merges the night in and publishes it (`make
compat` and `make drift` do the same locally, into `docs/`, where git ignores them). Its two
git steps are `compatgen/publish.sh restore` and `publish`, which `TestPublishTwoNights`
runs over two nights against a scratch origin. The digest,
`report/drift.md`, `drift.json` and `slack.json`, gives for each harness's newest build:

- **new surfaces**: surfaces no build had, once each with its keys counted, so a renamed
  span is one line, not one per key
- **new fields**: keys new to their surface, or new to the harness anywhere
- **removed fields** and **surfaces no longer sent**: what the newest build the night
  censused whole no longer sends that the newest older build censused whole did. Only a whole
  census is evidence: a partial one is a failed run, where keys of an error path come, so a
  night whose newest build is partial judges the newest whole one before it, or nothing, and
  says so. A build is judged once, on its first whole census, and only while no newer build
  has one: a re-run that did not reach an error path is no evidence. The digest names the
  builds judged and judged against where they are not the newest and the previous one
- **newly withheld fields**: unclassified keys the relay drops;
  classify each in `internal/relay/allow.go`, an agent's capture rules, or as content. Those
  still withheld from before are one reminder line until they are.

For a harness whose source is public (Codex, `sources` in `compatgen/source.go`; one line
adds another), `-source` links each of these to the lines of the build's source that name
it, read from its release tag's tarball, and a new build's headline links the comparison
of the two tags. Tests, test modules and comments (to the end of a line, or a block) are not
the build's source. A key is
linked beside its surface's name (or the constant that holds it): a line that names a key
belongs to the surface named nearest it in the same function, of all the harness is known to
send, so a generic key leads to its own metric or event, not to every line that says it nor
to another event in the same file; a key named nowhere beside its surface is linked
only if a few lines name it, and a name as common as `model` is left unlinked. A field or
surface gone says which it is: **still in** the new build's source beside its surface (it was
not sent tonight: a condition, a schedule, a scenario) or **gone from** it (removed), with
where the build before named it. Slack shows one link a finding, and none where the links
would cut a section short. A source that cannot be read leaves its findings unlinked, and
the digest says so. Nothing is read on a night with nothing to link.

It also lists capabilities whose result changed, and says so when the census did not run,
did not reach a harness censused within the week or whose census scenarios ran that night,
or did not reach the newest build: the
newest the catalog had, or the newest the night's census scenarios ran (`report/census.json`).
A census that did not run never reads as a
quiet night. The digest goes to the run's summary and to Slack through the
`SLACK_WEBHOOK_URL` secret in the `live-harnesses` environment, every night, so a quiet
channel means the job did not run. Drift never fails the night; a removed key on a surface
terma parses fails its golden.

## Versions

Each scenario runs against the installed binary and the last three releases
(`TERMA_E2E_CLAUDE_VERSIONS`, `TERMA_E2E_CODEX_VERSIONS`: `installed`, `lastN`,
or a comma list), oldest first. Version lists come from the npm registry;
binaries come from Claude Code's download service (manifest checksum) and
Codex's GitHub release tarballs (CLI and code-mode host), cached under `versions/`. A recording shim in
front of `terma` keeps every hook payload each build sends, so payload shapes
are golden files too. The complete Codex installation matters: the companion host makes file-edit hooks
work on 0.153.3 and 0.153.4 too. Missing edit events fail the attribution contract;
they must not count as a pass for an assumed unsupported version.

## Adding a harness

One driver file (start it, get past its dialogs, send a prompt, exit), one
test file with the contracts the matrix row promises, and golden files. Codex
uses `codex exec` for the API route, the developer's `~/.codex/auth.json`
copied into the scratch `CODEX_HOME` for the ChatGPT route, and terma's machine-wide
`CODEX_HOME/hooks.json`, which setup approves in the scratch config's `[hooks.state]`.

`TestMachineHooks*` covers where those hooks record: beside a repository's own Claude
Code, Codex and git hooks; the listed origin from a subdirectory, a differently named
checkout, its ssh and https forms and linked worktrees; nothing in a same-named folder
with another origin, a fork, a repository with no origin or a folder outside git; and a
Codex thread resumed into an unlisted repository.

## Cursor

`terma setup` offers Cursor as Coming Soon and writes no Cursor hooks, so every Cursor
scenario below is recorded as not run until it does.

`TestCursorHookDelivery` needs no provider credentials: it sends synthetic payloads
through installed hooks and the real Terma binary to the loopback OTLP receiver,
checking ordering, private-content exclusion and the kill switch. Run it with
`TERMA_E2E=1 TERMA_E2E_BINARY=../bin/terma go test -run '^TestCursorHookDelivery$' .`.

`TestCursorCLITurnObservations` runs two turns in one conversation through the
real CLI, checks installed hooks and their optional token snapshots against
OTLP delivery, and verifies the resulting commit attribution. It needs
`CURSOR_API_KEY` (a CLI key, not a team admin API key); missing credentials or a
missing binary are explicit skips. The CLI runs with a scratch home and repository.

```sh
# Build the root binary locally first: make build
TERMA_E2E=1 TERMA_E2E_BINARY=../bin/terma \
  TERMA_E2E_CURSOR_BINARY=/absolute/path/to/cursor-agent \
  go test -v -run '^TestCursorCLITurnObservations$' .
```

Supply the key through the environment, never a command argument. This verifies
CLI collection only, not plan classification, billing reconciliation, full token
coverage or IDE behavior. The same hooks serve the IDE;
its authenticated check remains manual.


`TestCursorBillingSession` is the narrower conversation-billing check: two real
file edits with the same resumed conversation, plus delivered session/account
identity. It does not require per-turn hooks. On 2026-09-16 this passed with CLI
`2026.09.10-fd3934a`; the strict turn-hook scenario failed because headless Cursor
emitted only lifecycle hooks. This is recorded as a provider coverage gap.

Set `TERMA_E2E_CURSOR_CAPTURE=report/cursor-billing-validation/session.json` when
running that test to save a private capture and the synthetic CLI responses.
The standalone billing importer (outside this repository) accepts this capture
and joins it to team billing using a separate `CURSOR_ADMIN_API_KEY`. A successful
capture can contain fewer lifecycle snapshots than CLI invocations; neither
lifecycle generations nor observation counts are a reliable turn counter.


`TestCursorInteractiveTurnObservations` runs two prompts in one pseudo-terminal
session and checks the same strict hook/token/order/file/commit contracts. It
passed on `2026.09.10-fd3934a` on 2026-09-16. The shared assertion waits for both
Stops to reach the receiver, because raw shim capture precedes detached delivery.
Use `make run RUN='^TestCursorInteractiveTurnObservations$'` (and the optional
binary/capture variables above). The inspected `--print` runner lacks the local
turn-hook invocations present in the interactive UI; the headless test remains a
separate regression check.

## Sandbox setup

Each sandbox runs `terma setup --team … --harness claude,codex --yes --no-browser
--relay-service off` with a local account fixture that supplies the developer login,
project list, and collection policy. Setup runs from the recording shim's path, since
the hooks it writes call terma by absolute path; the relay it starts and its check-in
are cleared before the scenario. The relay forwards only agents the profile records,
and setup records only Claude Code and Codex, so relay scenarios for other agents
record them in the profile themselves. Each scenario points its exporter separately, with a dummy key for the
loopback receiver. Account hosts are persisted in the private profile so relay
services load the same scoped policy after a restart. No real provider credentials
are needed for the deterministic telemetry scenarios.
Terma setup subprocesses have a 30-second deadline, so a login regression fails
promptly rather than consuming the entire suite timeout
(`TestSandboxSetupWithoutProviderCredentials` checks this offline).
