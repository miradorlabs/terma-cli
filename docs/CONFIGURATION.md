# Configuration and authentication

This guide covers the settings intentionally kept out of the repository: authentication,
profiles, agent telemetry, export scope, and environment variables. For the user-facing
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
saving a selection.

A linked Git worktree (`git worktree add`, including the ones coding agents create for
isolated work) uses its own `.terma/settings.json` when it has one, and otherwise its
main checkout's. That matters when the binding is gitignored: a new worktree does not
get ignored files, so without the fallback its hooks would have no project to report to.
The link is Git's own, never directory nesting — a separate repository inside a bound
one does not inherit it. Everything from a worktree reports as its main repository
(`repo`) with a `worktree` attribute naming it, and `terma status` / `terma doctor` say
when a worktree is bound through its main checkout.

## Local files

```text
~/.config/terma/
  config.json        profiles, organization, endpoints, preferred agents
  credentials.json   CLI credentials, one per organization and profile (0600)
  keys.json          project server keys and per-harness keys (0600)
  helpers/           the headers helpers that hand Claude Code its credential (0700)
  relay/             the relay's port and token, and what it has yet to deliver
  spool/             queued events
```

Project server keys are namespaced by project and never written to the repository.
`TERMA_API_KEY` supplies a key for CI and headless use.

## Agent telemetry

`terma setup` configures each agent's telemetry once, machine-wide, in the agent's own
global settings file — `~/.claude/settings.json` (or `$CLAUDE_CONFIG_DIR`) for Claude
Code, `~/.codex/config.toml` (or `$CODEX_HOME`) for Codex. Every launcher reads those
files: the CLI, Codex Desktop and the ChatGPT app, Claude Desktop, an IDE extension.

```sh
terma setup                                   # sign in, pick agents and this machine's project
terma setup --project "Acme Web"              # change the machine's project
terma setup --prompts off                     # stop sending prompt text and model responses
terma setup --signals traces,logs --exclude-tool-content
terma setup --no-relay                        # export straight to Terma (see below)
```

**Through the relay (the default on macOS and Linux).** The agents export OTLP/JSON to
terma's relay on `127.0.0.1`, authenticated with a token only this machine holds. The
relay sends each session to the project of the repository it ran in — the binding
`terma install` wrote — and a session outside any bound repository to the machine's
project. The agents' files name no project and hold no Terma key; keys stay in
`keys.json`. See [the relay](RELAY.md).

**Straight to Terma (`--no-relay`, or no launchd / systemd).** Each agent's file points
at Terma's ingest host with the machine project's key, so every session on the machine
reports to that one project. `terma status` and `terma doctor` say so in a repository
bound to another project.

A file that already exports to another collector is left alone and named; `terma setup
--force` replaces it. Terma's own earlier settings are always replaced. Restart running
agents after setup so they read the new settings. `terma install` configures the agents
itself when setup has not, with the repository's project as the machine's, and takes
the same export flags (`--prompts`, `--signals`, `--exclude-tool-content`, `--identity`,
`--no-relay`) as a change to the machine's choices.

Versions before the relay routed Claude Code and Codex per repository through PATH shims
and a block in the shell's startup file. `terma setup`, `terma install` and `terma
update --refresh` remove them, and `terma install` removes the telemetry policy an
earlier install committed to `.claude/settings.json` (it would outrank the machine's
configuration there). A shim left behind until then starts the agent unchanged.

## Export scope and consent

The advanced `terma connect` command writes one agent's telemetry configuration and
presents a checklist first. The controls are also available as flags:

```sh
terma connect claude \
  --signals traces,logs,metrics \
  --exclude-prompts \
  --exclude-tool-content \
  --scope global \
  --yes
```

`--signals none` disables export. A machine-wide connection can export everywhere or
require repositories to opt in with `--exports repos`, in which case a repository's
committed policy (`terma connect claude --scope local`) switches its signals on. `terma
setup` always configures the agents to export everywhere.

Your agents send prompt text and model responses by default: `terma setup --prompts off`
stops them and `--prompts on` turns them back on. The choice is the machine's, recorded
in `config.json`, and every later setup or install keeps it.

Codex ignores project-level OTEL configuration and has no headers helper, so its
configuration is always the user-level `config.toml`. In relay mode it carries only the
relay's token. Codex can suppress tool output but cannot suppress native tool arguments
with `--exclude-tool-content`. See [SECURITY.md](../SECURITY.md) for these limitations.
Codex Desktop reads the same file; select `codex-desktop` during `terma setup` so
`terma install` wires the Codex hooks that announce its sessions — see [Codex desktop
telemetry](CODEX-DESKTOP-TELEMETRY.md). OpenCode uses a plugin and
`.opencode/terma.json` rather than environment variables.

`terma doctor` fails when a connected agent has no telemetry signals enabled in the
current repository, even if another agent is configured correctly. For Claude it
combines user settings, `.claude/settings.json`, and `.claude/settings.local.json`,
including the master telemetry switch and the beta switch required for traces. `terma
status` uses the same checks for setup readiness. When an agent exports through the
relay, both also check the relay: installed, running, answering on loopback, and
delivering (a project held for a key, or the last failed delivery, is a warning).

Doctor and status list remaining setup actions rather than estimating a percentage
of spend from configuration. Doctor also compares the running executable with the
one hooks find on PATH: an older build may stamp commits while failing to read the
repository's current binding format. Resolve a binary mismatch before verifying
delivery. Backend read-back remains unverified when its API cannot confirm the event.
Private overrides must be edited in the named file. Restart the agent afterwards.

These are configuration checks, not proof that a running agent has emitted data.
Doctor's backend round-trip verifies Terma's hook events separately. Managed Claude
settings, custom `--settings` arguments, and a running session's inherited environment
are outside this configuration check.

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
that triggered the update. Windows requires a manual release download.

An explicit `terma update` upgrades a Homebrew or npm installation through the package
manager that owns it, read from where the binary lives: `<prefix>/bin/brew upgrade
[--cask] terma` for `<prefix>/Caskroom` or `<prefix>/Cellar`, and `npm install --global
--prefix <prefix> @miradorlabs/terma@latest` for `<prefix>/lib/node_modules` (that
prefix's npm, else the one on PATH). When that program cannot be found, or fails, the
command to run is printed instead. terma installed as a project's own npm dependency
(`<project>/node_modules`) is that project's to upgrade: `terma update` names
`npm install @miradorlabs/terma@latest` and the project directory, and runs nothing. Automatic updates never run a package manager; those
installations get notices only.

### Refreshing what terma installed

An update replaces the binary; what earlier versions wrote stays as it was until
something rewrites it. So once the new version is in place, `terma update` runs it as
`terma update --refresh`, which rewrites, with the new version's templates:

- the wrapped Claude Code status line (falling back to the renderer recorded in
  `statusline.json`),
- the OpenCode plugin, around its own configuration,
- and, in the repository it runs in, the commit hooks through the manager and the agent
  hook files for the adapters that `.terma/settings.json` records.

It also restarts the relay, so the new version serves, and removes the per-repository
PATH shims, shell startup block and routing records earlier versions installed.

It works only from what is on disk. It never signs in, never creates a file (one that is
gone was removed on purpose and stays gone; `terma install` brings it back), and never
changes a choice — unlike re-running `terma install`, which puts every flag it does not
record (`--no-statusline`, `--adapters`) back to its default. The repository files it changes are listed to
commit.

### Migrating saved state

When a new version changes the shape of something terma keeps in `~/.config/terma` — a
routing record, the key store, a status-line record — it ships a migration that rewrites
the old shape. Every terma command, hooks included, checks `migrations.json` when it
starts (one small read) and applies any migrations this version has that the machine has
not had, in order, before reading anything else. It needs no command from you, and
whichever process starts first after an update does it: the rest wait for it, briefly.
A hook never fails because of a migration; it stays silent and tries again on a later
run. An interactive command says what failed, `terma doctor` shows a `saved state
migrated` line while one is pending or failed, and `terma update --refresh` retries it
straight away and reports what it applied.

Migrations only add to or fill in what an earlier version wrote, so an older terma
still on the machine (doctor warns when there is one) keeps reading the same files.
They change nothing in a repository; committed files are `terma update --refresh`'s,
when you ask.

The first interactive command under a newer release, however it arrived (an automatic
update, or `brew upgrade` run by hand), refreshes the home-directory files once and
records the release in `refreshed.json`. It does not rewrite committed files: when the
current repository's hooks are out of date it says so, and `terma update --refresh` there
updates them. In any repository afterwards, `terma status` and `terma doctor` compare the
committed hooks with what this terma writes and name the same command when they differ.
`.terma/settings.json`'s `terma_version` records the terma that last wrote those files;
it changes only when install or a refresh rewrites one.

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
