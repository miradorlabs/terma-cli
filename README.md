<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/terma-logo-dark.svg">
    <img src="docs/assets/terma-logo-light.svg" alt="terma" width="200">
  </picture>
</p>

`terma` connects coding agents to [Terma](https://terma.ai), then stamps the commits
they produce so agent spend can be traced to shipped code.

## The workflow

Terma has two onboarding commands with different owners:

| Command | Run it | What it does |
|---|---|---|
| `terma setup` | Once per developer (optional) | Signs you in and records which coding agents you use. It does not bind a project or write repository files. |
| `terma install` | Once per repository | Binds the repository to a Terma project, configures per-repository agent routing, and offers to install commit and agent hooks. |

Repository telemetry is enabled by `terma install`; no extra telemetry flag is needed.
Restart running agents after installation so they load the new configuration.

Codex desktop uses a separate backend from the `codex` shell command. Select
**Codex Desktop** in `terma setup` (or use `--harness codex-desktop` with
`terma install`). Install then configures repository hooks and a local project
route. Trust the hooks in the app and check `terma desktop status` from that repository.
See [Codex desktop telemetry](docs/CODEX-DESKTOP-TELEMETRY.md).

Then verify the installation:

```bash
terma setup       # optional: sign in and choose agents
terma install     # run inside each repository
terma doctor      # verify the chain end to end
```

`terma install` can use `--project <name-or-id>`, an existing
`.terma/settings.json`, or an interactive picker. Use `--yes` for non-interactive
setup, or `--harness none` when you only want commit hooks. The committed settings
file contains a project reference, never a secret.

## Install

### Homebrew (macOS and Linux)

```bash
brew tap miradorlabs/tap
brew trust miradorlabs/tap     # once; Homebrew will not load an untrusted tap
brew install terma
```

### curl | bash

```bash
curl -fsSL https://terma.ai/install.sh | bash
```

The installer selects the platform archive, verifies `checksums.txt`, and installs
the static binary to `/usr/local/bin` or `~/.local/bin`. Set `TERMA_INSTALL_DIR` to
override the destination or `TERMA_VERSION=vX.Y.Z` to pin a release. The script is
POSIX `sh`, so `| sh` works too.

### npm, direct download, or source

```bash
npm install -g @miradorlabs/terma
```

Native binaries are also available from [Releases](https://github.com/miradorlabs/terma-cli/releases),
with checksums. From source:

```bash
make install
```

Terma checks for newer versions daily after interactive commands. Update notices are
on by default; automatic installation is opt-in:

```sh
terma update --check        # check without installing
terma update                # install the latest published release
terma update --auto on      # automatically install future releases
terma update --auto status  # show the saved preference
terma update --auto off     # return to notifications only
```

Updates verify the release checksum before replacing the binary. Hooks, launch shims,
CI, and scripted commands never trigger automatic updates. Homebrew/npm installations
receive an upgrade command for their package manager. A release binary carries its
tag, which the updater compares with the latest published release; a source build is
never updated without `terma update --force`.

Release, versioning and installer details are in [RELEASING.md](docs/RELEASING.md).

## Per-repository routing

To send two repositories to different Terma projects on one machine, `terma install`
creates a small directory of agent shims and puts it ahead of the real `claude` and
`codex` binaries on `PATH`. It asks to add that directory to the end of your shell
startup file (`~/.zshrc`, `~/.bashrc`, `~/.bash_profile`, or fish `conf.d`). Open a new
terminal afterward.

`terma doctor` detects when another startup-file entry has moved Terma behind the
real binary. `terma shim uninstall` removes the managed block; `--no-path` prints the
line instead, and `--activation wrapper` prints shell functions for users who prefer
not to use `PATH` shims.

An IDE extension that launches an agent by full path can bypass the shims and use the
machine-wide configuration. See [CONFIGURATION.md](docs/CONFIGURATION.md) for routing,
profiles, authentication, and export scope.

## What gets collected

Terma uses fast, local hooks. Each committed hook is a guarded one-liner that calls
`terma hook <event>`; the binary owns the session files, touched-file manifests,
commit trailers, and local event spool. On a machine without Terma, hooks are silent
and inert. Set `TERMA_HOOKS=0` to disable them on a machine where Terma is installed.

Commit attribution works like this:

1. Agent hooks announce sessions and record files the agent edits.
2. `prepare-commit-msg` intersects staged files with those manifests and adds one
   `Agent-Session-Id` / `Agent-Tool` trailer pair per matching session.
3. `post-commit` retires the committed files and records the commit event.

Hooks never make a network request. They append to a local queue, and delivery happens
after commits and session ends with retry and backoff. The prepare-commit-msg path is
tested against a sub-50 ms budget. Read the full behavior, hook-manager integration,
and spool guarantees in [INSTRUMENTATION.md](docs/INSTRUMENTATION.md) and
[DESIGN.md](docs/DESIGN.md).

## Supported agents

Terma currently supports Claude Code, Codex, Cursor, OpenCode, and Antigravity. The
support level differs by agent:

- Claude Code and Codex provide commit attribution plus native telemetry paths.
- OpenCode uses a dependency-free plugin for model, tool, session, and file events.
- Cursor provides commit attribution and ordered hook observations; billed-cost and
  quota mapping depend on platform integration.
- Antigravity provides session, turn, tool, and file observations but has no token or
  cost export.

See the adapter contracts for exact event names, privacy boundaries, and verification
status: [Cursor](docs/CURSOR-INSTRUMENTATION.md),
[Antigravity](docs/ANTIGRAVITY-INSTRUMENTATION.md),
[Codex and subagents](docs/SUBAGENT-INSTRUMENTATION.md), and
[funding evidence](docs/FUNDING-INSTRUMENTATION.md).

## Read usage and attribution

```bash
terma status
terma usage --user dawson --since today
terma session list --user dawson --since yesterday
terma blame <commit>
```

`status` is the quick local view of sign-in, project binding, hooks, connected
agents, queue state, and remaining setup steps. `doctor` performs the end-to-end check,
including a scratch commit in a temporary worktree. `usage`, `session`, and
`principal` use the active profile and repository's project; output automatically becomes JSON
when stdout is not a terminal. See [INSIGHTS.md](docs/INSIGHTS.md) for time windows,
filters, pagination, and JSON semantics.

Organization and project names are shown without UUIDs in normal output. IDs remain
available with `terma org list -o json` and `terma project list -o json`; lists and
pickers show them when a name is missing or duplicated.

Choose the project for each repository with `terma install`. Reads use that repository's
binding; use `--project <id>` for a one-command override or when outside a repository.
Switching organizations never changes a repository's project.

## Privacy and security

Authentication uses a browser handoff with PKCE and a loopback callback. Credentials
and project keys stay in the user's configuration directory with restrictive file
permissions; repository settings contain no secrets. Export choices support signal,
prompt, tool-content, and global-versus-local scope controls.

Read [SECURITY.md](SECURITY.md) for the threat model, consent behavior, credential
storage, hook guarantees, and reporting instructions.

## Commands

The normal command surface is intentionally small:

```text
setup       Sign in and choose agents
install     Configure this repository
status      Show local connections, queue, and setup readiness
doctor      Verify the full chain
session     Inspect agent sessions
usage       Summarize usage and cost
blame       Trace a commit to an agent session
org         List and switch organizations
uninstall   Remove repository installation files
update      Update terma
```

Authentication, direct telemetry management, project lookup, principal lookup,
configuration, hook execution, shim management, and spool maintenance remain
available as hidden commands for automation and troubleshooting. Run
`terma <command> --help` for details.

## Documentation

- [Configuration and authentication](docs/CONFIGURATION.md) — profiles, credentials, project routing, export scope, and environment variables
- [Instrumentation guide](docs/INSTRUMENTATION.md) — hooks, commit stamping, queues, status lines, and hook managers
- [Insights](docs/INSIGHTS.md) — usage, sessions, principals, filters, and machine-readable output
- [Design notes](docs/DESIGN.md) — the decisions behind the hook path and its performance guarantees
- [Security](SECURITY.md) — login flow, privacy boundaries, and threat model
- [Development](docs/DEVELOPMENT.md) — build, test, lint, benchmark, and dev-backend workflows
- [Releasing](docs/RELEASING.md) — versioning, release assets, signing, installers, and smoke tests
- [Cursor instrumentation](docs/CURSOR-INSTRUMENTATION.md)
- [Antigravity instrumentation](docs/ANTIGRAVITY-INSTRUMENTATION.md)
- [Subagent instrumentation](docs/SUBAGENT-INSTRUMENTATION.md)
- [Funding instrumentation](docs/FUNDING-INSTRUMENTATION.md)
- [Agent-facing CLI guide](https://terma.ai/cli/llms.txt)

## License

MIT — see [LICENSE](LICENSE).
