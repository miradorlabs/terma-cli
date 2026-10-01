# Architecture

terma is one binary with three roles. A developer runs its **command line**. The coding
agents and git run its **hooks**. A **local relay daemon** receives the agents' own
telemetry and forwards only what an opted-in repository claimed. Each coding agent is a
**plugin**: one package behind the interfaces in `internal/agents`, registered in one
place.

```
                  ┌──────────────────────────── terma (cmd/terma) ────────────────────────────┐
 developer ──────▶│ internal/cli          the command tree, run by cli.App                    │
                  │   ├─ internal/install   one plan: dry run prints it, Apply carries it out │
                  │   ├─ internal/doctor    the checks, in order; probes reach network/spool  │
                  │   └─ internal/connect   an agent's exporter, machine-wide or per repo     │
                  │                                                                           │
 agent hooks ────▶│ terma hook <event> → hooks/dispatch → hooks/hookrun → internal/spool ─────┼──▶ Terma ingest
 git hooks   ────▶│   (prepare-commit-msg stamps trailers: internal/trailer, internal/session)│
                  │        │ claims sessions (internal/relay/claim)                           │
                  │        ▼                                                                  │
 agent OTLP  ────▶│ terma relay run  →  internal/relay/daemon  →  internal/relay ──── outbox ─┼──▶ Terma OTLP
 (127.0.0.1)      │                       (policy refresh, keys,   (route, redact,            │
                  │                        heartbeat, service)      deliver)                  │
                  └───────────────────────────────────────────────────────────────────────────┘
                                ▲ every agent-specific answer comes from
                  internal/agents (contract + registry) ◀── internal/agents/builtin ◀── internal/agents/<name>
```

## The three roles

**The command line** (`internal/cli`). `cmd/terma/main.go` hands `cli.New` the
registry `builtin.Agents()` returns and the version `-ldflags` stamps, then exits with
`App.Execute`'s status. Everything a command reads, it reads through the `App`: the
agents, the version, the global flags, and the seams tests replace. The package holds no
other state. Commands stay thin: `install` builds an `install.Plan` and applies it,
`doctor` runs `doctor.Run`, and `telemetry connect` runs `connect.Global` or
`connect.Local`. Each one supplies what its package must not reach itself, such as
sign-in, key minting and the network, as `install.Steps`, `doctor.Probes` or
`connect.Steps`.

**The hooks** (`internal/hooks`). A committed hooks file calls `terma hook <event>`,
guarded so that a machine without terma does nothing. `dispatch` runs it: the git hooks
first, then the owning agent's render hook, its hooks-off handler or its event handler
(from the registry's event index), then the claim, the relay start and the flush, which
the command line injects. `hookrun` is the runtime every event shares: a bounded payload reader, the session lifecycle (start, files touched,
end), the evidence attributes, and the spool append. An agent's handlers build on it
from the agent's own package. `hookmgr` plans the files that wire hooks: the commit
hooks through whichever manager the repository uses (husky, lefthook, pre-commit or
terma's own shims), and the event-keyed JSON the agents read. `prepare-commit-msg`
stays local and fast: no network, and one `git diff --cached` only when a manifest
exists. Events leave through `internal/spool`, a JSONL queue with backoff per project.

**The relay daemon** (`internal/relay`, `internal/relay/daemon`). The agents' exporters
send to `127.0.0.1:43180`. The engine (`relay`) holds a record until a hook in a bound
repository claims its session, applies that project's content policy, and writes the
record to the route's outbox before answering. One sender per route then delivers it.
Unclaimed records never touch disk. The daemon (`relay/daemon`) is the process around
the engine: its state directory, the run loop, `Spawn` and `Stop` for hooks, the Windows
supervisor and the per-user service, the policy for a claim (`Resolver`), key minting,
the heartbeat, and `PolicyRefresher`, which keeps each team's collection policy fresh
while it runs. `daemon.Deps.Engine` assembles all of it; the command line hands it only
what reaches the network or the registry (minting a key, sending a heartbeat, fetching a
policy, the agents' declarations), so the daemon is tested with fakes. The relay never imports an agent. What it needs of one, it gets through
`relay.Options`: how records name their session (`shape.Correlator`), where they carry
content (`shape.Capturer`), and which keys are safe to pass.

## Agents as plugins

Every coding agent lives in `internal/agents/<name>`, and `internal/agents/builtin` is
the only place that lists them. Each agent implements `agents.Agent`: its name, its
committed hooks file and the plan that writes it, the `terma hook` events it handles,
and which of those start a flush. Everything else is an optional capability that the
registry finds by type (`Registry.With[C]()`, `Registry.Find[C](name)`, Go 1.27 generic
methods). Examples are trust in committed hooks, machine-wide hooks, surfaces (a CLI and
a desktop app chosen apart), the relay exporter, the status line, and the notifier.
`internal/agents/doc.go` is the full map. Each agent package asserts the capabilities it
implements (`var _ agents.X = Agent{}`), so a method that drifts fails the build instead
of silently switching off.

`agents.Agent` is the one plugin contract. `harness.Harness` is not a second hierarchy
beside it. It is the interface of the exporter-configuration kit (`internal/harness`):
connect, status, scope, the ownership journal and the settings writes. An agent whose
own settings file holds an OTLP exporter uses that kit and hands its `Harness` over
through the `agents.Exporting` capability. Nothing registers a harness, and nothing
reaches one except through its agent. The registry's harness lookups (`Harnesses`,
`HarnessNames`, `Harness(name)`) are one-line projections over `With[agents.Exporting]`;
`Harness(name)` also normalizes a name a developer typed and words the error that lists
the choices. Agents without such an exporter, such as Pi or Gemini through the relay or
Cursor with hooks only, have no harness at all.

Adding an agent means adding one package, plus one line in `builtin`. What it commits
to is pinned where a reviewer sees it change: its hook event names
(`internal/agents/builtin`'s tests), the safe keys it adds to the relay's union
(`internal/relay/safe_pin_test.go`), and the bytes of every file it writes
(`internal/contract`).
`internal/contract` keeps byte snapshots of every file terma writes for an agent:
committed hooks, machine-wide hooks, managed configuration and relay exporter settings.

Supported agents are Claude Code (CLI and Desktop) and Codex (CLI and Desktop). The
others are registered so that their hooks and telemetry keep working where they are
wired, but `install` does not wire them by default.

## Package map

| Layer | Packages |
|---|---|
| Entry point | `cmd/terma` |
| Command line | `internal/cli` |
| Workflows | `internal/install`, `internal/doctor`, `internal/connect`, `internal/globalmode`, `internal/refresh` |
| Agents | `internal/agents` (contract, registry), `internal/agents/builtin`, `internal/agents/<name>`, `internal/agents/internal/*` (shared by a few agents), `internal/agents/agentstest` (a made-up agent for workflow tests) |
| Hook runtime | `internal/hooks/dispatch`, `internal/hooks/hookrun`, `internal/hooks/hookmgr`, `internal/hooks/hookruntest` |
| Relay | `internal/relay` (engine), `internal/relay/daemon`, `internal/relay/claim`, `internal/relay/shape`, `internal/relay/service` |
| Events and attribution | `internal/spool`, `internal/session`, `internal/trailer` |
| Exporter kit | `internal/harness` (exporter configuration, journal, settings writes) |
| Account | `internal/account/api`, `internal/account/auth`, `internal/account/keystore`, `internal/account/serverkey` |
| Terminal | `internal/ui/style`, `internal/ui/output`, `internal/ui/prompt`, `internal/ui/spinner` |
| Collection policy | `internal/policy` (fetched with the developer's login; `internal/routing` keeps the validated cache) |
| State and plumbing | `internal/config`, `internal/project`, `internal/routing`, `internal/migrate`, `internal/selfupdate`, `internal/gitx`, `internal/flock`, `internal/procinfo`, `internal/shellrc` |
| Guards (tests only) | `internal/boundary`, `internal/contract` |

## What the tests enforce

`internal/boundary` turns this document into failing tests:

- Only `builtin` imports an agent's package, and no agent imports another.
- Nothing outside `internal/agents` names an agent, by identifier, in a string or in a comment. Run
  `go test ./internal/boundary -mentions` to list each mention it finds.
- Every directory under `internal/agents` is registered.
- Import directions:
  - The relay imports neither `internal/harness` nor `internal/agents`.
  - The relay engine (the `internal/relay` package itself) imports no `internal/routing`: it
    takes a resolved `relay.Policy`, which the daemon builds (`daemon.CapturePolicy`).
  - `doctor` imports neither `account/api`, `spool` nor the relay daemon.
  - The relay, daemon included, imports neither `account/api` nor `internal/policy`.
  - `install` imports nothing under `account` and not `spool`; `connect` neither, nor
    `install`, which never imports `connect` either.
  - `hookrun` and `hookmgr` never import each other.
  - The hook runtime (`hookrun`, `hookmgr`, `hookruntest`) imports no agent, command,
    account or exporter-kit package. `dispatch` reads the registry, and imports neither the
    command line, the account packages, the exporter kit nor the daemon. The registry never
    imports `dispatch`.
  - An agent never reads core state: the routing record, the relay's claims, keys and
    credentials reach it as facts (`hookrun.Repo.Route`, `hookrun.Consent`,
    `agents.SurfaceInput`), so `internal/agents` imports no `routing`, `relay/claim`,
    `account/keystore`, `account/auth` or `account/api`.
  - The account packages import nothing but the platform's own.
  - The terminal packages import nothing of terma's.
  - Only `cmd/terma` imports `internal/cli` and `builtin`.
- `internal/cli` has no subdirectories, and no package-level variable of its own is
  ever assigned or has its address taken, tests included.

The relay pins the union of the safe keys every agent declares as a literal list
(`internal/relay/safe_pin_test.go`), and content always outranks safe. The commands a
message, the README or `docs/` tells someone to run must exist
(`internal/cli/command_surface_test.go`).
