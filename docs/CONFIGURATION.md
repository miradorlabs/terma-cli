# Configuration and authentication

This guide covers the settings intentionally kept out of the repository: authentication,
profiles, project routing, telemetry scope, and environment variables. For the user-facing
setup path, start with the [README](../README.md).

## Sign in

`terma setup` (or the advanced `terma login`) opens the Terma browser handoff with a PKCE
challenge. You choose an organization and approve access; a single-use code is returned to
a listener on `127.0.0.1` and redeemed with a verifier that never leaves the CLI process.

Credentials are organization-scoped and stored with restrictive permissions in
`~/.config/terma/credentials.json`. The directory honors `XDG_CONFIG_HOME` and can be
replaced with `TERMA_CONFIG_DIR`. Existing sessions are reused. `terma login --force`
replaces the current session, and `terma logout` revokes stored sessions server-side.

Multiple organizations can be managed locally:

```sh
terma org list
terma org use "Beta Labs"
terma login --org beta
terma logout
```

Projects are selected separately in each repository with `terma install` (or
`terma install --project "My Project"`). The choice is saved in `.terma/settings.json`
and reused on reinstall. Switching organizations changes your account scope without
choosing a project or changing repository bindings.

Project-scoped reads use the current Git repository's binding, including from its
subdirectories. Outside an installed repository, use `--project <id>` for a single
command. An explicit `--project` or `TERMA_PROJECT_ID` overrides the binding without
saving a selection. Legacy project defaults in user profiles are ignored; login
clears them. The old `terma project use` command directs you to `terma install`.

## Local files

```text
~/.config/terma/
  config.json        profiles, organization, endpoints, preferred agents
  credentials.json   CLI credentials, one per organization and profile (0600)
  keys.json          project server keys and per-harness keys (0600)
  spool/             queued events
```

Project server keys are namespaced by project and never written to the repository.
`TERMA_API_KEY` supplies a key for CI and headless use.

## Agent routing

`terma install` stores a repository's project binding and prepares agent launch routing:

- Claude Code receives a per-repository settings document through `claude --settings`.
- Codex receives telemetry as launch-time `-c` overrides while retaining its original
  `CODEX_HOME`, login, history, trust, and notifier configuration.
- Both routes are delivered by PATH shims, or by shell functions printed with
  `--activation wrapper`.

The shim directory must appear before the machine-wide agent binary. `terma install` offers
to append a marked block to the end of the shell startup file; later PATH edits can
otherwise put the real binary first. `terma doctor` reports when routing is configured but
not active.

An IDE or launcher that invokes an agent by absolute path bypasses the shim and uses
machine-wide configuration. `doctor` also checks for a different Terma build elsewhere on
the machine.

Routing can be bypassed explicitly for one launch:

```sh
TERMA_DISABLE=1 claude
TERMA_DISABLE=1 codex
```

## Export scope and consent

The advanced `terma connect` command presents a checklist before writing telemetry
configuration. The controls are also available as flags:

```sh
terma connect claude \
  --signals traces,logs,metrics \
  --exclude-prompts \
  --exclude-tool-content \
  --scope global \
  --yes
```

`--signals none` disables export. A machine-wide connection can export everywhere or
require repositories to opt in with `--exports repos`. `terma install` enables repository telemetry by default, including for this arrangement.
It writes a reviewable policy alongside the agent hooks, keeping endpoints and keys out
of the repository. Reinstalling preserves an existing policy; use `--signals`,
`--exclude-prompts`, and `--exclude-tool-content` to change what the repository sends.
Restart Claude Code after installing so the session loads the new settings.

Codex ignores project-level OTEL configuration, so direct `terma connect codex` is
machine-wide. `terma install` instead routes each launch using runtime `-c` overrides
from the repository binding, as described above. Those overrides include the telemetry
bearer key, which local process inspection can expose; do not log generated agent
arguments. Codex can suppress tool output but cannot suppress native tool arguments
with `--exclude-tool-content`. See [SECURITY.md](../SECURITY.md) for these limitations.
OpenCode uses a
plugin and `.opencode/terma.json` rather than environment variables.

`terma doctor` fails when a connected agent has no telemetry signals enabled in the
current repository, even if another agent is configured correctly. For Claude it
combines user settings, `.claude/settings.json`, and `.claude/settings.local.json`,
including the master telemetry switch and the beta switch required for traces.
Live shim routing is checked against its own signal list, since its launch settings
override those files. `terma status` uses the same checks for setup readiness.

Doctor and status list remaining setup actions rather than estimating a percentage
of spend from configuration. Doctor also compares the running executable with the
one hooks find on PATH: an older build may stamp commits while failing to read the
repository's current binding format. Resolve a binary mismatch before verifying
delivery. Backend read-back remains unverified when its API cannot confirm the event.
A missing repository policy is repaired with `terma install`; an existing disabled
policy requires an explicit choice, such as `terma install --signals traces,logs,metrics`.
Private overrides must be edited in the named file. Restart the agent afterwards.

These are configuration checks, not proof that a running agent has emitted data.
Doctor's backend round-trip verifies Terma's hook events separately. Managed Claude
settings, custom `--settings` arguments, and a running session's inherited environment
are outside this configuration check.

Shell activation has its own diagnostic in both commands. It reports a missing
per-project route, a missing PATH setup or inactive wrapper, a startup file that
requires a new terminal, and a later PATH entry that bypasses the shims. It warns
even when global telemetry still works, and says which settings provide that
fallback. OpenCode needs no shell integration. Diagnostics never opt in or modify
your shell startup file; `terma install` offers that setup.

## Updates

`terma update --check` checks immediately. Ordinary successful interactive commands
check once a day per installed version after a successful lookup and print notices
on stderr. Failed checks retry after 15 minutes, including when no public release is
available yet. Explicit `terma update --check` always checks immediately.

`terma update --auto on` opts into installing published releases after those commands;
`--auto off` restores notifications only, and `--auto status` shows the preference.
The preference is machine-wide in `~/.config/terma/updates.json`, independent of the
active account profile. `update-check.json` stores only versions and timestamps. Concurrent update attempts are serialized.

Automatic updates use checksum verification and an atomic binary replacement. A
failed download leaves the installed binary intact and does not fail the command
that triggered the update. Homebrew/npm installations receive instructions to update
through that package manager; Windows requires a manual release download.

Update checks compare the release tag stamped into the binary with the latest
published release. Source builds (`make build` reports `git describe`, a plain
`go build` reports `dev`) are skipped by passive checks; `terma update --force`
switches one to a published release.

Set `TERMA_NO_UPDATE_CHECK=1` to suppress all passive update work; explicit
`terma update` commands still work.

## Environment variables

| Variable | Purpose |
|---|---|
| `TERMA_API_KEY` | Server key for CI and headless commands |
| `TERMA_CONFIG_DIR` | Replace the default configuration directory |
| `TERMA_PROFILE` | Select a configuration profile |
| `TERMA_ENV` | Select the hidden `prod`, `dev`, or `local` environment |
| `TERMA_API_URL` | Override the data API endpoint |
| `TERMA_AUTH_URL` | Override the authentication endpoint |
| `TERMA_APP_URL` | Override the browser application URL |
| `TERMA_OTLP_URL` | Override the OTLP ingest endpoint |
| `TERMA_NO_UPDATE_CHECK=1` | Disable passive update checks and automatic installation |
| `TERMA_DEBUG=1` | Explain hook behavior on stderr |
| `TERMA_HOOKS=0` | Disable all Terma hooks immediately |
| `TERMA_STATUSLINE_TIMEOUT` | Extend the Claude status-line renderer timeout |

Endpoint and environment overrides are intended for development and self-hosted
deployments; production is the default and the only environment shown in normal help.
