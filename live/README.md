# live

Real harnesses, real credentials, the real `terma` binary, against the
collection matrix in `docs/collection-matrix.html`. A cell there is either a
passing test here or a claim.

```sh
make test                      # offline value contracts; no harnesses or credentials
cp .env.example .env.local     # fill in what you have
make live                      # build in Docker, run natively, write report/latest.md
make live RUN=TestClaudeSubscriptionSession
LIVE_UPDATE_GOLDEN=1 make live # re-record the attribute key sets after a harness upgrade
```

Every scenario runs in a sandbox: scratch Terma config, scratch harness
config, and a scratch git repository that `terma install` and `terma connect`
configured exactly as they would for a developer, with the export pointed at an
OTLP receiver inside the test. Contracts then read three planes together:

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
compatibility; `make live` runs those scenarios and propagates a failing test's
exit status to the caller.

## Credentials and modes

| variable | enables | route |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | isolated Claude Code sessions on a subscription | subscription |
| `TERMA_LIVE_REAL_LOGIN=1` | the same with the developer's own login (no token needed) | subscription |
| `ANTHROPIC_API_KEY` | headless Claude Code on the Console route | api_key |
| `OPENAI_API_KEY` | Codex on the API route | api_key |

A missing credential makes its scenarios "not run" in the report. Nothing here
passes by absence.

`TERMA_ENV` is the one terma setting the sandboxes inherit (`TERMA_ENV=dev make live …`),
so anything that is not the in-test receiver stays off production. The sandbox installs
with `--harness none --no-browser`: it has no account, and a sign-in that crept back into
`terma install` fails the run instead of opening a browser.

For Codex API tests, the suite pipes the key to `codex login --with-api-key` in
its scratch `CODEX_HOME`, using `cli_auth_credentials_store="file"` for login and
execution. Supplying `OPENAI_API_KEY` alone left requests unauthenticated on the
tested builds. The temporary profile is removed after the test; the key is never
put in command arguments. See [Codex authentication](https://learn.chatgpt.com/docs/auth).

Both authentic API routes passed on 2026-09-15: Claude 2.1.270–2.1.272 and Codex
0.153.3–0.154.0. Run only these scenarios with:

```sh
TERMA_LIVE_REPORT=report/api-routes make live RUN='^(TestClaudeAPIKeyHeadless|TestCodexAPIKey)$'
```

`TestClaudeIsolatedRenderer` and `TestClaudeStopFailureDelivery` use real Claude
binaries with controlled loopback responses and dummy keys. They verify installed
hook/renderer behavior independently of provider credentials; they do not verify
the authentic API billing route.

## CI

The main CI workflow runs this module's offline contracts with the race detector.
`Live harness contracts` runs nightly and on manual dispatch against the latest
three releases. Claude uses GitHub OIDC with Anthropic workload identity
federation; no Anthropic API-key secret is needed. The workflow contains the
non-secret federation, organization, service-account and workspace IDs. Each
Claude process gets a fresh identity token in a private temporary file and
exchanges it through Claude Code's native federation support. These short
headless scenarios use API billing, not subscription credentials.

The Anthropic rule is configured for inference only (`workspace:inference`) in
workspace `terma-cli-ci` (`wrkspc_01Ug36g2VPKLUSFZ9LZXFTvU`), with a maximum token
lifetime of 600 seconds. Its match requires all of:

- Audience `https://api.anthropic.com`.
- Subject `repo:miradorlabs@243301318/terma-cli@1360515068:environment:live-harnesses`.
- Repository `miradorlabs/terma-cli`, repository ID `1360515068`, owner ID `243301318`.
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

The OpenAI API key is supplied only to credential preflight and live execution, not
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
noted in the report so the matrix can grow. Re-record with `LIVE_UPDATE_GOLDEN=1`
after checking what changed.

## Versions

Each scenario runs against the installed binary and the last three releases
(`TERMA_LIVE_CLAUDE_VERSIONS`, `TERMA_LIVE_CODEX_VERSIONS`: `installed`, `lastN`,
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
copied into the scratch `CODEX_HOME` for the ChatGPT route, `.codex/hooks.json`
trusted in the scratch config's `[hooks.state]`.

## Cursor

`TestCursorHookDelivery` needs no provider credentials: it sends synthetic payloads
through installed hooks and the real Terma binary to the loopback OTLP receiver,
checking ordering, private-content exclusion and the kill switch. Run it with
`TERMA_LIVE=1 TERMA_LIVE_BINARY=../bin/terma go test -run '^TestCursorHookDelivery$' .`.

`TestCursorCLITurnObservations` runs two turns in one conversation through the
real CLI, checks installed hooks and their optional token snapshots against
OTLP delivery, and verifies the resulting commit attribution. It needs
`CURSOR_API_KEY` (a CLI key, not a team admin API key); missing credentials or a
missing binary are explicit skips. The CLI runs with a scratch home and repository.

```sh
# Build the root binary locally first: make build
TERMA_LIVE=1 TERMA_LIVE_BINARY=../bin/terma \
  TERMA_LIVE_CURSOR_BINARY=/absolute/path/to/cursor-agent \
  go test -v -run '^TestCursorCLITurnObservations$' .
```

Supply the key through the environment, never a command argument. This verifies
CLI collection only, not plan classification, billing reconciliation, full token
coverage or IDE behavior. The same project hooks are installed for the IDE;
its authenticated live check remains manual (see `docs/CURSOR-INSTRUMENTATION.md`
in the parent repository).


`TestCursorBillingSession` is the narrower conversation-billing check: two real
file edits with the same resumed conversation, plus delivered session/account
identity. It does not require per-turn hooks. On 2026-09-16 this passed with CLI
`2026.09.10-fd3934a`; the strict turn-hook scenario failed because headless Cursor
emitted only lifecycle hooks. This is recorded as a provider coverage gap.

Set `TERMA_LIVE_CURSOR_CAPTURE=report/cursor-billing-validation/session.json` when
running that test to save a private capture and the synthetic CLI responses.
The [standalone importer](../pocs/cursor-billing/README.md) accepts this capture
and joins it to team billing using a separate `CURSOR_ADMIN_API_KEY`. A successful
capture can contain fewer lifecycle snapshots than CLI invocations; neither
lifecycle generations nor observation counts are a reliable turn counter.


`TestCursorInteractiveTurnObservations` runs two prompts in one pseudo-terminal
session and checks the same strict hook/token/order/file/commit contracts. It
passed on `2026.09.10-fd3934a` on 2026-09-16. The shared assertion waits for both
Stops to reach the receiver, because raw shim capture precedes detached delivery.
Use `make live RUN='^TestCursorInteractiveTurnObservations$'` (and the optional
binary/capture variables above). The inspected `--print` runner lacks the local
turn-hook invocations present in the interactive UI; the headless test remains a
separate regression check.
