<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/terma-logo-dark.svg">
    <img src="docs/assets/terma-logo-light.svg" alt="terma" width="200">
  </picture>
</p>

The `terma` cli connects coding agents to [Terma](https://terma.ai), then stamps the commits
they produce so agent spend can be traced to shipped code.

## The workflow

Terma has four commands, and every one is safe to run again. One of them onboards, once
per developer:

```bash
terma setup       # sign in, choose your team and agents, write machine-wide hooks
terma doctor      # verify the chain end to end
```

`terma setup` signs you in, chooses your team (`--team <name-or-id>`, else your
organization's only team, else a picker), records which coding agents you use, fetches
the team's collection policy, points those agents at the local relay, and writes the
agents' machine-wide hooks and git's global `core.hooksPath`. It writes nothing into a
repository's working tree or committed files; a clone with its own hooks path (husky's,
say) gets a git config entry routing it through terma's hooks, which `terma teardown`
removes. Run it again to repair the machine, to switch team, or, with `--org`, to
switch organization.

`terma doctor` checks the result, `terma update` keeps terma current, and `terma
teardown` undoes setup on the machine. Restart running agents after setup so they load
the new configuration.

Each agent is one choice for its CLI and its desktop app, which share the agent's
user-level settings: **Claude Code & Desktop** and **Codex TUI & Desktop**. Setup
approves terma's own Codex hooks itself.

### Which repositories are collected

Your team's collection policy, set in Terma, lists the repositories it collects. A
session or commit is recorded only in a repository the list names; everywhere else the
hooks write nothing.

Each entry is a repository as `host/owner/name`, e.g. `github.com/miradorlabs/mirador-platform`.
The CLI admits a session only inside a git working copy whose `origin` remote, normalised,
equals an entry ignoring case: host lowercased without port or credentials, path with
`.git` and any trailing slash stripped, from scp (`git@host:path`), `https://`, `ssh://`
and `git://` forms. A linked worktree reads `origin` from its main repository. A folder
outside git, a repository with no `origin`, or an `origin` that is a local path or
`file://` URL is never admitted. An entry has a host and at least two path segments, none
empty; a longer path matches a longer `origin` path exactly (GitLab subgroups). An empty
list admits nothing.

So a clone in any folder, a subdirectory of it, and a worktree of it at
`.claude/worktrees/fix-1` are all collected by `github.com/acme/api` when origin is
`git@github.com:acme/api.git` or `https://github.com/acme/api`; a fork whose origin is
`github.com/you/api` is not. An SSH host alias (`git@github-work:acme/api` through
`~/.ssh/config`) normalises to host `github-work` and does not match
`github.com/acme/api`; `terma doctor` shows the origin terma sees. In global mode the
policy collects every folder, inside git or not. Each session reports to the team of the
developer who ran it, so two developers on different teams in one repository each report
to their own.

### Hook managers

terma's global git hooks run each repository's own hooks after terma's, so hooks a hook
manager already installed keep running.

- **husky** sets the clone's own `core.hooksPath`, which outranks git's global one. The
  first agent session in that clone adds a git config entry routing it through terma's
  hooks, which then run husky's; `terma teardown` removes the entry.
- **pre-commit** refuses to install while a global `core.hooksPath` is set, and
  **lefthook** prints a notice and installs nothing. Install them, or add a hook type,
  with git's global config out of the way: `GIT_CONFIG_GLOBAL=/dev/null pre-commit
  install`, or `GIT_CONFIG_GLOBAL=/dev/null lefthook install`. They install into
  `.git/hooks`, which terma's hooks run. `terma doctor` points this out for pre-commit.
  Avoid `lefthook install --force` and `--reset-hooks-path`: they overwrite or unset
  terma's global hooks.

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
terma update                # install the latest release, or refresh what terma installed
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
update, the new version also refreshes what earlier versions wrote in your home
directory — the wrapped Claude Code status line, the OpenCode plugin — keeping every
choice you made. It works from what is on disk, never signs in, and never adds a file.
On the latest release, `terma update` does just that refresh.

## Routing

Your agents export to a relay terma runs on your machine (on `127.0.0.1`), from their own
user-level settings — which is also what Claude Desktop, Codex Desktop and IDE extensions
read, so they are covered too. A machine-wide hook claims each session that runs in a
repository your team collects, for your team; the relay forwards only claimed sessions,
with your team's key. Everything else — personal work, other repositories, folders
outside git — waits briefly and is dropped: it never leaves your machine. While it
waits it is also kept on your disk, readable only by you, so a relay restart does not
lose it; it is deleted as soon as it is sent or dropped. A
session that moves to a repository the list does not name, or whose repository leaves
the list, stops being forwarded.

What content leaves is your team's collection policy, set in Terma, and nothing else.
The relay and the hook queue's delivery fetch it with your login; until they have,
nothing they would send leaves. Agents send prompts, model responses and tool input and
output to the relay, and the relay removes what the policy does not collect before
anything leaves. Hook events, which queue on this machine and never pass the relay, are
held to the same policy when they are sent: an event queued before the policy tightened
leaves without the content it no longer collects, and one from a repository no longer
listed does not leave.

`terma setup` runs the relay as a per-user background service, so it is up before any
agent starts; with `--relay-service off`, hooks start it on demand. `terma update`
rewrites a service an earlier terma wrote. `terma doctor` shows whether it runs and
delivers; `terma teardown` stops it and removes its service.

## What gets collected

Terma uses fast, local hooks. Each machine-wide hook is a guarded one-liner that calls
terma by the full path setup wrote; the binary owns the session files, touched-file
manifests, commit trailers, and local event spool. If terma is gone, the hooks are
silent and inert. `TERMA_HOOKS=0` turns the hooks off for one shell or process. Git's
global hooks chain to the hooks git ran before, so a repository's own hooks still run;
a repository that sets its own `core.hooksPath` outranks them, which `terma doctor`
reports.

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
attribution through machine-wide hooks, and each agent's native telemetry through the
local relay.

## Check the setup

```bash
terma doctor
```

`doctor` opens with what this machine collects and what the repository has in progress
(the active session, uncommitted agent edits), then checks every link end to end —
sign-in, whether the team collects this repository (and the origin terma sees), hooks, the agents' export, the queue,
and delivery to the backend. Every failure names its fix.

Organization and team names are shown without UUIDs in normal output; pickers show the
ID when a name is missing or duplicated, and a name that matches nothing lists yours.

Reads use the team you chose at setup; `--team <id>` overrides it for one command.

## Privacy and security

Authentication uses a browser handoff with PKCE and a loopback callback. Credentials
and team keys stay in the user's configuration directory with restrictive file
permissions; none is written into a repository. What content leaves is the team's
collection policy alone, applied on this machine before anything is sent.

## Commands

The command surface is intentionally small, and every command is safe to run again:

```text
setup       Sign in, choose team and agents, write machine-wide hooks (--org switches)
doctor      Verify the full chain
update      Update terma, or refresh what it installed
teardown    Undo setup on this machine (--sign-out also signs out)
```

Hidden commands remain for the programs that run them — hook execution, the local
relay, spool delivery, shell completion — and for Terma's own engineers.

## Architecture

terma is one binary with three roles:

- the **command line** (`internal/cli`), which `cmd/terma` starts;
- the **hooks** that coding agents and git run machine-wide (`terma hook <event>`,
  `internal/hooks`);
- a **local relay daemon** (`terma relay run`, `internal/relay`). It forwards an
  agent's own telemetry only for sessions a hook claimed in a repository the team
  collects.

Each coding agent is a plugin: one package under `internal/agents/<name>`, behind the
interfaces in `internal/agents`, and registered in `internal/agents/builtin`. Nothing
else names an agent, and `internal/boundary` tests that this holds.

## Documentation

- [Agent-facing CLI guide](https://terma.ai/cli/llms.txt)

## License

MIT — see [LICENSE](LICENSE).
