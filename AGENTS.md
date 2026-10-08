# AGENTS.md

This file provides guidance to coding agents (Claude Code, Codex and others) when working
with code in this repository.

`terma` is a Go CLI that connects coding agents (Claude Code, Codex) to Terma and stamps
the commits they produce with the agent session that wrote them.

## Commands

```bash
make build              # → bin/terma
make check              # gofmt, vet, lint, the semconv registry, every test; before committing
make format             # go fix + gofmt -w
make lint               # pinned golangci-lint; doc comments required on exported names
make bench-hook         # prepare-commit-msg must stay under 50 ms
make semconv            # render internal/semconv from semconv/registry (needs weaver)
go test ./internal/hooks/hookrun/ -run TestName
cd test/e2e && make run   # real agent binaries; needs their credentials
```

Run everything with `TERMA_ENV=dev` (the Makefile sets it). Without it, `terma setup` signs
in against production. A test that can reach a login passes
`--no-browser`.

## Layout

```text
cmd/terma/              entry point
internal/cli/           the command line; `terma --help` lists only the primary commands
internal/agents/        one package per coding agent; builtin/ is the only list of them
internal/hooks/         `terma hook <event>`: dispatch, runtime (hookrun), hook-file writers (hookmgr)
internal/relay/         the local relay daemon
internal/spool/         the local queue hooks append to
internal/delivery/      sends the spool to each project, from a detached flush
internal/session/       session manifests and commit attribution
internal/doctor/        `terma doctor` checks
internal/setup/         `terma setup`; internal/globalmode/ writes the machine-wide hooks
internal/repohooks/     the commit hooks a claimed session installs in a repository's own
                        .git/hooks, chained to whatever was there
internal/semconv/       event names and attribute keys, rendered from semconv/registry
internal/boundary/      tests that enforce this layout
test/e2e/               tests against the real agent binaries (its own Go module)
semconv/                the Weaver registry of what terma sends, its policies and templates
npm/, install.sh        distribution
```

## How it fits together

**Hooks.** `terma setup` writes each agent's user-level hooks. The git hooks are
installed per repository, on demand: the first agent session claimed in a repository the
policy collects, with the policy's `git_hooks` on, writes `prepare-commit-msg`,
`post-commit` and `pre-push` into that repository's own `.git/hooks` — the common git directory's, so a
linked worktree shares the one install; hooks already there are left as they are. Nothing
in terma ever takes them out, the policy switched off and teardown included: once terma is
gone they only run the hook each one displaced. git runs one file per hook name, so a hook already
there is moved to `<name>.pre-terma` and terma's script runs it first, unchanged and
keeping its veto; a repository that sets its own `core.hooksPath` is skipped, and `terma
doctor` says so. Nothing is written into a repository's working tree or committed files:
`.git/hooks` is neither, and git never tracks it. Every hook runs `terma hook <event>`, which first resolves
the working copy's `origin` as `host/path` (`gitx.RepositoryFS`) and asks the team
policy's repository list (`config.Policy.Admits`); in repository mode, in a repository the list does not
name, or outside git, it writes no manifest, spool line, trailer or claim for a project,
only a placement with no project that marks the session not collected (`claim.Mark`), and
under a policy no relay has refreshed for minutes it also starts a refresh. Each
session is claimed for the developer's own team from setup.

**Commits.** The agent hooks announce the session and record the files it edits in a
session manifest. At commit time, `prepare-commit-msg` matches the staged files against
those manifests and adds an `Agent-Session-Id` / `Agent-Tool` trailer for each matching
session. `post-commit` retires the committed files and records the commit. `pre-push`
records the refs git is about to push and hands them to a detached `terma spool
await-push`, which waits for `git push` to exit, reads whether the remote-tracking branch
moved, walks the pushed commits and spools one `terma.push` per branch. Hooks never
touch the network: they append events to the spool, and a detached `terma spool flush`
delivers them to each project. `internal/boundary` keeps `net/http` out of the hook, spool
and agent packages.

**Files.** The config directory (`config.Dir`) holds what the developer chose and what
setup changed: `config.json` (profiles and settings), `credentials.json`, `keys.json`, and
`setup/` (each change setup made to another tool's files, with what it replaced). The
credentials' secrets, sign-in tokens and server keys, live in the system keychain
(`internal/account/secret`), as gh keeps its token: `credentials.json` and `keys.json`
index them, and hold them only where there is no keychain or `--insecure-storage` asks.
The state directory (`config.StateDir`) holds what terma writes as it runs: `relay/`,
`spool/`, `policies/`, `agents/<agent>/` (each agent's hook state), `funding/`,
`workspaces/` (session stores for work begun outside git) and `update/`; a repository's
session store is `.git/terma/`. Names are lowercase with dashes; `.json`, `.log` and
`.lock` say what a file holds, a single plain value has no extension, a folder of per-key
files is plural, a name never repeats its folder's, and state that belongs to one agent
sits under its name. `App.Execute` resolves both directories once and passes them down;
nothing below it reads them from the environment (`internal/boundary`).

**Telemetry.** Claude Code and Codex ignore exporter settings in repository config, and
desktop apps and IDE extensions read only user-level settings. But a user-level exporter
sends everything, personal work included, under one key. So every agent's user-level
exporter points at a relay that terma runs on `127.0.0.1`. A hook in a collected repository
claims its session for the developer's team, naming the repository. The relay forwards
only claimed sessions, only from the processes the claim names, and only while the policy
still lists the claim's repository. It sends them with the team's key and applies the team's
collection policy: agents export all content to it, and the policy alone decides what
leaves (`config.Policy.Content`). The claim also names the working tree the hook ran in
and the directory in it, which the relay stamps on the session's records as
`terma.repository.root` and `terma.working_directory`, with tool content only. Hook events never pass the relay, so delivery applies
the same rules when it sends them. A session marked not collected is dropped on arrival;
everything else unclaimed is held briefly and dropped: nothing unclaimed leaves the
machine. The exception is a policy in global mode, which collects every session on the
machine: it admits every repository, a hook outside git claims its session for the
policy's default project, and the relay sends what no claim places there too, except a
session an agent's hooks claim every one of (`shape.SessionKey.Claimed`): it waits for its
claim, so a hidden side thread no hook claims stays here. What is held stays in memory,
never on disk, and a stopping relay drops it.

## Rules

- Hook entries only call `terma hook <event>`, guarded so they do nothing once terma is
  gone. All logic lives in the binary.
- `prepare-commit-msg` stays local: no network, at most one git subprocess.
- Only `internal/agents` names a specific agent. A new agent is a new package plus a line
  in `internal/agents/builtin`.
- Wire names are contracts with other repositories, so never rename them: the commit
  trailers, hook event names, and every event and attribute terma sends. The Weaver
  registry in `semconv/registry` is their source of truth; `make semconv` renders
  `internal/semconv` from it, and code uses those constants, never a literal.
- Nothing terma does writes into a repository's working tree or committed files. Its git
  hooks go under `.git/hooks`, which git neither tracks nor carries in a commit.
  What a developer collects is the
  team policy's repository list, read through `config.Policy.Admits` alone.
- Help text never mentions the hidden `dev` and `local` environments.
