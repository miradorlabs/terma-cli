<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/terma-logo-dark.svg">
    <img src="docs/assets/terma-logo-light.svg" alt="terma" width="200">
  </picture>
</p>

The `terma` cli connects coding agents to [Terma](https://terma.ai), then stamps the commits
they produce so agent spend can be traced to shipped code.

## The workflow

Terma has two onboarding commands with different owners:

| Command | Run it | What it does |
|---|---|---|
| `terma setup` | Once per developer (optional) | Signs you in, records which coding agents you use, fetches your organization's collection policy, and points those agents at the local relay. It writes no repository files. |
| `terma install` | Once per repository | Binds the repository to a Terma team, configures per-repository agent routing, and offers to install commit and agent hooks. |

Repository telemetry is enabled by `terma install`; no extra telemetry flag is needed.
Run it from any subdirectory: Git worktrees and submodules use their own root.
Outside Git, the first install uses the current directory; later install/uninstall
calls from subdirectories find the nearest `.terma/settings.json`. Agent hooks and
telemetry work in these folders, while Git hooks and commit stamping are skipped.
If you later run `git init` in that same folder, rerun `terma install` to add Git
hooks; existing sessions and tracked edits carry on without losing attribution.
Bare repositories are rejected because they have no working directory.

Restart running agents after installation so they load the new configuration.

Each agent is one choice for its CLI and its desktop app, which share the agent's
user-level settings: **Claude Code & Desktop** and **Codex TUI & Desktop**. Codex runs
Terma's hooks once you approve them with `/hooks` (in the desktop app: Settings → Hooks →
Review).

Then verify the installation:

```bash
terma setup       # optional: sign in and choose agents
terma install     # run inside each repository
terma doctor      # verify the chain end to end
```

`terma install` binds the repository to a team: the only one, when your
organization has one; otherwise it asks, offering the one in an existing
`.terma/settings.json` first (Enter keeps it), or takes `--team <name-or-id>`. A bound team your account cannot see is never used:
install says why and lets you choose another. Use `--yes` for non-interactive setup
(it keeps an existing binding), or `--harness none` when you only want commit hooks.
It shows the team, prompt-capture setting, warnings, and what is left for you
to do; `-v` / `--verbose` also shows setup steps and every file and setting it wrote. The committed settings
file contains a team reference, never a secret.

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
terma update --refresh      # bring this repository's hooks up to the installed version
terma update --auto on      # automatically install future releases
terma update --auto status  # show the saved preference
terma update --auto off     # return to notifications only
```

Updates verify the release checksum before replacing the binary. Hooks, the local
relay, CI, and scripted commands never trigger automatic updates. `terma update` upgrades a
Homebrew or npm installation through the package manager that owns it. A release binary
carries its tag, which the updater compares with the latest published release; a source
build is never updated without `terma update --force`.

The first time a new version runs, it migrates anything it keeps in `~/.config/terma`
whose format changed, before doing anything else, with no command from you. After an
update, the new version also refreshes what earlier versions wrote — the wrapped Claude
Code status line, the OpenCode plugin, and the hooks of the repository you ran `terma
update` in — keeping every choice you made when you installed. It works
from what is on disk, never signs in, and never adds a file. The repository hooks are
committed files, so they change only when you ask: run `terma update --refresh` in each
other repository to bring its hooks up to date, then commit them.

## Per-repository routing

Your agents export to a relay terma runs on your machine (on `127.0.0.1`), from their own
user-level settings — which is also what Claude Desktop, Codex Desktop and IDE extensions
read, so they are covered too. The relay forwards a session only when a hook in a
repository you ran `terma install` in claimed it, and sends it to that repository's
team with that team's key. Everything else — personal work, other repositories —
waits briefly in memory and is dropped: it never leaves your machine. Prompts and model
responses are sent by default; `terma install --prompts off` stops them for a team,
and the relay removes them before anything leaves.

`terma setup` and `terma install` run the relay as a per-user background service, so it
is up before any agent starts; with `--relay-service off`, hooks start it on demand.
`terma relay status` shows whether it runs and what it has forwarded, dropped and queued.

## What gets collected

Terma uses fast, local hooks. Each committed hook is a guarded one-liner that calls
`terma hook <event>`; the binary owns the session files, touched-file manifests,
commit trailers, and local event spool. On a machine without Terma, hooks are silent
and inert. `terma pause` stops all capture on the machine, hooks and the relay alike,
until `terma resume`; events already queued are still delivered. `TERMA_HOOKS=0` turns
the hooks off for one shell or process.

Commit attribution works like this:

1. Agent hooks announce sessions and record files the agent edits.
2. `prepare-commit-msg` intersects staged files with those manifests and adds one
   `Agent-Session-Id` / `Agent-Tool` trailer pair per matching session.
3. `post-commit` retires the committed files and records the commit event.

Hooks never make a network request. They append to a local queue, and delivery happens
after commits and session ends with retry and backoff. The prepare-commit-msg path is
tested against a sub-50 ms budget.

## Supported agents

Terma supports Claude Code (CLI and Desktop) and Codex (CLI and Desktop): commit
attribution through repository hooks, and each agent's native telemetry through the local
relay.

## Check the setup

```bash
terma status
terma doctor
```

`status` is the quick local view of sign-in, team binding, hooks, connected
agents, queue state, and remaining setup steps. `doctor` performs the end-to-end check,
including a scratch commit in a temporary worktree. Output automatically becomes JSON
when stdout is not a terminal.

Organization and team names are shown without UUIDs in normal output. IDs remain
available with `terma org list -o json` and `terma team list -o json`; lists and
pickers show them when a name is missing or duplicated.

Choose the team for each repository with `terma install`. Reads use that repository's
binding; use `--team <id>` for a one-command override or when outside a repository.
Switching organizations never changes a repository's team.

## Privacy and security

Authentication uses a browser handoff with PKCE and a loopback callback. Credentials
and team keys stay in the user's configuration directory with restrictive file
permissions; repository settings contain no secrets. Export choices support signal,
prompt, tool-content, and global-versus-local scope controls.

## Commands

The normal command surface is intentionally small:

```text
setup       Sign in and choose agents
install     Configure this repository
status      Show local connections, queue, and setup readiness
doctor      Verify the full chain
org         List and switch organizations
uninstall   Remove repository installation files
update      Update terma
```

Authentication, direct telemetry management, team lookup,
shell completion, configuration, hook execution, the local relay, and spool maintenance remain
available as hidden commands for automation and troubleshooting. Run
`terma <command> --help` for details.

## Architecture

terma is one binary with three roles:

- the **command line** (`internal/cli`), which `cmd/terma` starts;
- the **hooks** that coding agents and git run (`terma hook <event>`, `internal/hooks`);
- a **local relay daemon** (`terma relay run`, `internal/relay`). It forwards an
  agent's own telemetry only for sessions an opted-in repository claimed.

Each coding agent is a plugin: one package under `internal/agents/<name>`, behind the
interfaces in `internal/agents`, and registered in `internal/agents/builtin`. Nothing
else names an agent, and `internal/boundary` tests that this holds.

## Documentation

- [Agent-facing CLI guide](https://terma.ai/cli/llms.txt)

## License

MIT — see [LICENSE](LICENSE).
