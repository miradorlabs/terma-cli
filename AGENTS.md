# AGENTS.md

This file provides guidance to coding agents (Claude Code, Codex and others) when working
with code in this repository.

`terma` is a Go CLI that connects coding agents (Claude Code, Codex) to Terma and stamps
the commits they produce with the agent session that wrote them.

## Commands

```bash
make build              # → bin/terma
make check              # gofmt, vet, lint, every test; run it before committing
make format             # go fix + gofmt -w
make lint               # pinned golangci-lint; doc comments required on exported names
make bench-hook         # prepare-commit-msg must stay under 50 ms
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
internal/spool/         the local queue hooks append to; detached flushes deliver it
internal/session/       session manifests and commit attribution
internal/doctor/        `terma doctor` checks
internal/setup/         `terma setup`; internal/globalmode/ writes the machine-wide hooks
internal/boundary/      tests that enforce this layout
internal/account/, internal/config/, internal/harness/, internal/ui/output/
                        forked from ../mirador-cli; keep them close to it
test/e2e/               tests against the real agent binaries (its own Go module)
npm/, install.sh        distribution
```

## How it fits together

**Hooks.** `terma setup` writes each agent's user-level hooks and points git's global
`core.hooksPath` at terma's hooks, which chain to whatever git ran before. Nothing is
written into a repository's working tree or committed files; a clone with its own hooks
path gets a git config entry routing it through terma's hooks (teardown removes it).
Every hook runs `terma hook <event>`, which first resolves
the working copy's `origin` as `host/path` (`gitx.RepositoryFS`) and asks the team
policy's repository list (`config.Policy.Admits`); in a repository the list does not
name, or outside git, it writes nothing. Each session is claimed for the developer's own team from setup.

**Commits.** The agent hooks announce the session and record the files it edits in a
session manifest. At commit time, `prepare-commit-msg` matches the staged files against
those manifests and adds an `Agent-Session-Id` / `Agent-Tool` trailer for each matching
session. `post-commit` retires the committed files and records the commit. Hooks never
touch the network: they append events to the spool, and a detached `terma spool flush`
delivers them to each project.

**Telemetry.** Claude Code and Codex ignore exporter settings in repository config, and
desktop apps and IDE extensions read only user-level settings. But a user-level exporter
sends everything, personal work included, under one key. So every agent's user-level
exporter points at a relay that terma runs on `127.0.0.1`. A hook in a collected repository
claims its session for the developer's team, naming the repository. The relay forwards
only claimed sessions, only from the processes the claim names, and only while the policy
still lists the claim's repository. It sends them with the team's key and applies the team's
collection policy: agents export all content to it, and the policy alone decides what
leaves (`config.Policy.Content`). Hook events never pass the relay, so delivery applies
the same rules when it sends them. Everything else is held briefly and dropped: nothing
unclaimed leaves the machine. What is held is mirrored on disk, written behind and
removed as it leaves the hold, so a restarted relay takes it back for the rest of its
hold, judged by the collection mode it arrived under.

## Rules

- Hook entries only call `terma hook <event>`, guarded so they do nothing once terma is
  gone. All logic lives in the binary.
- `prepare-commit-msg` stays local: no network, at most one git subprocess.
- Only `internal/agents` names a specific agent. A new agent is a new package plus a line
  in `internal/agents/builtin`.
- Wire names are contracts with other repositories, so never rename them: the commit
  trailers, hook event names, and spool event names (`internal/hooks/hookrun/events.go`).
- Nothing terma does writes into a repository's working tree or committed files. What a developer collects is the
  team policy's repository list, read through `config.Policy.Admits` alone.
- Help text never mentions the hidden `dev` and `local` environments.
