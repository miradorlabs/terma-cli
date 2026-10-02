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
make test-install-e2e   # the built CLI installing into scratch repositories
make bench-hook         # prepare-commit-msg must stay under 50 ms
go test ./internal/hooks/hookrun/ -run TestName
cd test/e2e && make run   # real agent binaries; needs their credentials
```

Run everything with `TERMA_ENV=dev` (the Makefile sets it). Without it, `terma setup` and
`terma install` sign in against production. A test that can reach a login passes
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
internal/install/       per-repository install; internal/setup/ is the per-machine half
internal/boundary/      tests that enforce this layout
internal/account/, internal/config/, internal/harness/, internal/ui/output/
                        forked from ../mirador-cli; keep them close to it
test/e2e/               tests against the real agent binaries (its own Go module)
npm/, install.sh        distribution
```

## How it fits together

**Commits.** An agent's committed hooks run `terma hook <event>`. The hooks announce the
session and record the files it edits in a session manifest. At commit time,
`prepare-commit-msg` matches the staged files against those manifests and adds an
`Agent-Session-Id` / `Agent-Tool` trailer for each matching session. `post-commit` retires
the committed files and records the commit. Hooks never touch the network: they append
events to the spool, and a detached `terma spool flush` delivers them to each project.

**Telemetry.** Claude Code and Codex ignore exporter settings in repository config, and
desktop apps and IDE extensions read only user-level settings. But a user-level exporter
sends everything, personal work and other repositories included, under one key. So every
agent's user-level exporter points at a relay that terma runs on `127.0.0.1`. Hooks in an
installed repository claim each session they see. The relay forwards only claimed
sessions, and only from the processes the claim names. It sends them to that repository's
project with the project's key and applies the team's collection policy: agents export
all content to it, and the policy alone decides what leaves (`config.Policy.Content`).
Hook events never pass the relay, so delivery applies the same rule when it sends them. Everything else
is held briefly in memory and dropped: nothing unclaimed leaves the machine.

## Rules

- Committed hook files only call `terma hook <event>`, guarded so they do nothing on a
  machine without terma. All logic lives in the binary.
- `prepare-commit-msg` stays local: no network, at most one git subprocess.
- Only `internal/agents` names a specific agent. A new agent is a new package plus a line
  in `internal/agents/builtin`.
- Wire names are contracts with other repositories, so never rename them: the commit
  trailers, hook event names, and spool event names (`internal/hooks/hookrun/events.go`).
- `.terma/settings.json` is committed: no secrets, nothing per-developer.
- Install is a plain installation of hooks and routing: it never fetches, stores or reads
  the team's collection policy, and writes no content setting. The relay and the spool's
  delivery fetch the policy (`internal/policy`) and apply it; setup fetches it for global
  mode.
- Help text never mentions the hidden `dev` and `local` environments.
