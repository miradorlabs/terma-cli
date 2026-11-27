# AGENTS.md

This file provides guidance to coding agents (Claude Code, Codex and others) when working
with code in this repository.

`terma` is a Go CLI that connects coding agents (Claude Code, Codex, GitHub Copilot, OMP,
Antigravity, Grok Build, OpenCode) to Terma and stamps the commits they produce with the agent session that
wrote them.

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
internal/routing/       multi-org routing table (`routing.State` CAS, `routing.Router`)
internal/runs/          per-session run ledger and cross-org Mixed cutoff
internal/spool/         the local queue hooks append to
internal/delivery/      sends the spool to each project, from a detached flush
internal/history/       `terma history upload`: discovery, routing, holdings skip check, paced upload and resume state
internal/session/       session manifests and commit attribution
internal/doctor/        `terma doctor` checks (teams, routing state, Mixed runs, outbox/spool losses)
internal/setup/         `terma setup`; internal/globalmode/ writes the machine-wide hooks
internal/repohooks/     the commit hooks a claimed session installs in a repository's own
                        .git/hooks, chained to whatever was there
internal/shellrc/        the developer's shell startup files: install.sh's PATH line, and the
                        function that starts an agent's command through `terma launch`
internal/semconv/       event names and attribute keys, rendered from semconv/registry
internal/boundary/      tests that enforce this layout
test/e2e/               tests against the real agent binaries (its own Go module)
semconv/                the Weaver registry of what terma sends, its policies and templates
npm/, install.sh        distribution
```

## How it fits together

**Hooks.** `terma setup` writes each agent's user-level hooks. The git hooks are
installed per repository, on demand: the first agent session claimed in a repository a
selected team collects, with that team policy's `git_hooks` on, writes `prepare-commit-msg`,
`post-commit` and `pre-push` into that repository's own `.git/hooks` — the common git directory's, so a
linked worktree shares the one install; hooks already there are left as they are. Nothing
in terma ever takes them out, the policy switched off and teardown included: once terma is
gone they only run the hook each one displaced. git runs one file per hook name, so a hook already
there is moved to `<name>.pre-terma` and terma's script runs it first, unchanged and
keeping its veto; a repository that sets its own `core.hooksPath` is skipped, and `terma
doctor` says so. Nothing is written into a repository's working tree or committed files:
`.git/hooks` is neither, and git never tracks it. Every hook runs `terma hook <event>`, which resolves
the working copy's `origin` as `host/path` (`gitx.RepositoryFS`) and routes against the
profile's account-stamped `routing.State` (`routing.Router.Route`): the selected
organization teams' policies are tried newest first (`Policy.UpdatedAt`), a Refused team is
passed over, the first that lists the repository collects it, and personal comes last. Missing, unreadable, or
wrong-account routing state fails closed (zero fallback to `Profile.Team`). Unmatched
locations write no manifest, spool line, trailer, or team claim — only a not-collected
placement (`claim.Mark`). Read/shell reach hooks (and Extension agents' `<prefix>-tool-reach`)
call `runs.MarkMixed` when a session crosses organizations; from `MixedAt` onward all
conversation and telemetry content is dropped, while metadata-only `runs.Locational` events
(commits, pushes, file touches) still route by their own checkout. A folder outside git that
is no one's workspace crosses into no organization, Personal included: the filesystem's
root, a device or kernel filesystem (`2>/dev/null`), the operating system's own folders and
the machine's scratch space (`/tmp`, `$TMPDIR`), unless it is in the developer's home
(`nobodys` in `internal/hooks/hookrun/reach.go`). A reach hook that fails never blocks or
fails the agent's tool: an Extension agent's plugin taints the session, hands nothing more
of it over and asks terma to mark its run Mixed. A harness may run the entries in another
agent's hooks file (the grok agent's grok runs Claude Code's and Cursor's), so an agent
marks the hook processes it starts (`agents.HookStarter`), and `terma hook` stands down in
a process a harness other than the event's owner started: no claim, no spool line, no reply.
The git hooks are not subject to it.

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
setup changed: `config.json` (profiles and settings), `user-credentials.json` (each
profile's one sign-in: a user session of the person, which reads every organization and
team they belong to), `keys.json` (only a server key a profile was set up with,
`TERMA_API_KEY`), `installation-id` (this installation's stable id, sent at sign-in) and
`setup/` (each change setup made to another tool's files, with what it replaced). The
credentials' secrets, sign-in tokens and server keys, live in the system keychain
(`internal/account/secret`), as gh keeps its token: `user-credentials.json` and `keys.json`
index them, and hold them only where there is no keychain or `--insecure-storage` asks. An
older terma's per-organization `credentials.json` sessions are revoked and deleted at the
next sign-in, and the team keys it minted are deleted from this machine (a user token
cannot revoke them; an admin can, in the web app).
The state directory (`config.StateDir`) holds what terma writes as it runs: `routing/`
(per-profile `routing/<profile>.json` storing account-stamped team enrollments, cached
policies, updated via account- and generation-checked CAS
`routing.Update`/`routing.Replace`; only `terma setup` writes a profile's first one, but
for a server-key profile an earlier terma set up, whose stored team, key and policy the
refresh after an update, or a relay of this build as it starts, turns into its one-team
`no_person` state, once: `internal/cli/serverkey_migrate.go`), `runs/` (per-session run
ledgers and Mixed cutoffs),
`relay/` (including `outbox-v2/.teams/@<account>/<enrollment>/<tool>/`), `spool/`,
`policies/`, `agents/<agent>/` (each agent's hook state), `funding/`,
`workspaces/` (session stores for work begun outside git) and `update/`; a repository's
session store is `.git/terma/`, which also keeps what a git hook in an agent's sandbox could
not write to the state directory, until a hook outside the sandbox moves it there (a
sandboxed hook without `routing.State` parks with an empty destination and unpark drops and
counts `no_destination`). Names are lowercase with dashes; `.json`, `.log` and `.lock` say
what a file holds, a single plain value has no extension, a folder of per-key files is plural,
a name never repeats its folder's, and state that belongs to one agent sits under its name.
`App.Execute` resolves both directories once and passes them down; nothing below it reads
them from the environment (`internal/boundary`).

**Telemetry & multi-org routing.** Claude Code and Codex ignore exporter settings in
repository config, and desktop apps and IDE extensions read only user-level settings. So
every agent's user-level exporter points at a relay that terma runs on `127.0.0.1` (writing
`daemon.RunInfo` with `Routing: true` so hooks refuse pre-routing relays). An agent whose
command reads its exporter from its environment alone (`agents.Launched`: the Copilot CLI)
is launched via `terma launch <agent>` (`shellrc.InstallLauncher`). A hook claims its session
for the routed enrollment, naming the repository and checkout (`stampCheckout`,
`stampParent`), and on parts of an agent that writes the home directory as `~` in exported
paths (`shape.CaptureRules.HomeRelativeScopes`: grok's) also stamps that directory with
tool content (`terma.home_directory`, `Options.HomeDir`). A subagent's claim names the
session that opened it where the agent's own telemetry does not (OMP's and grok's,
`hookrun.Env.StartSubagent`), and grok's `PostToolUse` hook sends the call that launched a
subagent (`terma.subagent.call`) and, when the parent waits on or kills a background one,
how it ended (`terma.subagent.end`). The relay forwards only claimed sessions from claimed
PIDs, pinned once routed: unclaimed records wait briefly in `unknownHold`, while
blocked records stay destination-pinned in `pinnedHold` (re-checked only, never
re-routed), and spans/logs/metrics straddling `MixedAt` drop. It sends each team's backlog
with the person's single user token and `X-Mirador-Project: <team_id>` (a server-key profile
uses its `keys.json` server key under `Account: "no_person"` with no `X-Mirador-Project`),
applying that team's `config.Policy.Content`. On ingest `403 ORG_NOT_ACCESSIBLE`, the relay
triggers a coalesced refresh nudge and refuses all enrollments of that org via epoch-fenced
CAS (`routing.ApplyOrgAccessLost`) without retiring the user token, leaving other
organizations and Personal uninterrupted. Spool delivery applies the same lane,
`routing.State`, Mixed, and content rules. Antigravity has no export, so its hooks send its
content too: the prompt and the reply from the transcript, a tool call's arguments from
`PostToolUse` and each tool's result as `terma.tool.result`
(`internal/agents/antigravity/content_paths.go` and `turn.go`), under the content switches
of the team routing sends the hook's checkout to (`hookrun.Env.PolicyFor`) and only for
workspaces routing sends to that same team (`hookrun.Env.RoutesWith`). grok's export often
loses the prompt, so its prompt hook sends it as `terma.user.message` under the routed
team's policy (`PolicyFor`) and consent (`Consent`). OpenCode has no export of its own:
terma's plugin (`internal/agents/opencode/plugin/terma.js`, one file with OpenCode 1's `server`
entry and OpenCode 2's `setup` feeding one emitter) is its exporter, under scope
`terma-opencode`, and its hook caller, and claims each session the first time a process sees
it. Use `terma teams` to manage ticked teams, `terma route explain` to trace
`routing.Router.Route` for a path, and
`terma doctor` to inspect routing state, per-enrollment health (including `OrgAccessLost`,
policy conflicts, and single `terma setup` remediation for `SignInNeeded`), Mixed runs, and
combined outbox/spool losses.

### Lanes, ownership, and loss accounting

- **Account-bound lanes.** Everything that leaves is bound to the sign-in in place when it
  arrived (a 16-hex-digit hash of auth URL + user id): the relay outbox stores it under
  `@<account>/` and spool events carry `account`. A lane leaves only with that account's own
  token, never in another person's name, and `terma.user.id` names only that lane's user.
  Server-key profiles collect into `@no_person/` / `no_person` and leave only under a server
  key (switching to a user sign-in leaves that backlog to age out); a server key sends nothing
  else, and a route bound to no lane is never sent.
- **Fail closed on missing or unreadable sign-in.** Config (`config.json`, which pairs the
  selected organization/team with `Profile.Account`) and the sign-in are read as one snapshot.
  In the relay, a claimed part arriving signed out or unreadable (after up to ~3 s of retry
  for mid-write swaps) is never written and is counted as `signed_out` or `unreadable`. In
  hooks, `spool.LaneOf` binds the spool and refuses untagged writes: signed-out or unreadable
  events are dropped before writing any queue line, checkpoint, push record, or park file, and
  appended whole to `spool/losses/` (one file per day, so concurrent hooks never lose a count).
  Any untagged line found queued or parked is dropped.
- **Policy hold and outbox aging.** A claimed part whose team policy or key the relay can't
  read (not fetched yet, locked keychain, changed organization, or removed profile) waits in
  memory (`policyHold`) for up to 15 minutes while policy is fetched, then is dropped and
  counted as `policy_timeout` (also when dropped for room or on stop; updates stop waiting for
  policy-only parts past their pause wait). Outbox lanes age out after 14 days, with relay
  losses tracked in `losses.json`; `terma doctor` combines them with `spool/losses/`, and
  heartbeats report both (`dropped.<reason>.<signal>` and `dropped.<reason>.hooks` via
  `spool.TakeNewLosses`). Replays (`AppendAsCollected`) preserve their recorded lane.
- **Per-session atomic ownership.** Every session manifest write (locked or unlocked delta)
  first claims `<id>.owner` atomically via temp file + sync + `os.Link` (with an
  `O_CREATE|O_EXCL` fallback). The first claim decides the session's owner for good (`""` for
  a manifest written signed out is never filled in later). A hook under another account, or a
  session whose owner file or manifest can't be read, fails closed (`ErrAccountConflict`) and
  is counted as `account_conflict`. `Prune` skips when `store.lock` is busy, and unlocked
  delta writes re-claim after landing their delta so pruning cannot hand an active session to
  another account.
- **Git hooks and sandboxes.** A git hook's commit or push record uses its collector's lane
  unless a session's manifest names another account. A sandboxed git hook (which can read
  neither credentials nor write the state directory) takes the single lane agreed upon by all
  its sessions' manifests (never `session.json`), parks tagged records and loss counts under
  `.git/terma/` (`events-v2`, `pushes-v2`, `losses-v2`), and lets the next unsandboxed hook
  unpark and import counts once. Multi-account or unowned sandboxed records are dropped as
  `ambiguous_push`.
- **v2 queue isolation from v0.3.x.** This version writes only `spool/events-v2.jsonl`,
  `pushes-v2/`, `relay/outbox-v2/`, and `.git/terma/events-v2` / `pushes-v2`, never v0.3.x
  paths (`events.jsonl`, `pushes/`, `outbox/`, `.git/terma/events`, `pushes`). Neither version
  reads the other's queues, so an older relay outliving an update by hours (or an older install
  running alongside this one) cannot send v0.4 records with its own credential. 0.3.x is
  maintained on `release/v0.3.x`, and everyone signs in again at rollout.

## Rules

- Hook entries only call `terma hook <event>`, guarded so they do nothing once terma is
  gone. All logic lives in the binary.
- `prepare-commit-msg` stays local: no network, at most one git subprocess.
- Only `internal/agents` names a specific agent. A new agent is a new package plus a line
  in `internal/agents/builtin`.
- `internal/routing/testdata/entry_grammar.json` is a contract with the platform: which policy entries the account
  service stores and how `ParseEntry` reads them. The platform keeps a byte-identical copy and fails CI when it
  differs, so change it here first. The server stores nothing a released CLI would refuse or read differently, except
  under a new policy major, as 2.0 did for wildcards.
- Wire names are contracts with other repositories, so never rename them: the commit
  trailers, hook event names, and every event and attribute terma sends. The Weaver
  registry in `semconv/registry` is their source of truth; `make semconv` renders
  `internal/semconv` from it, and code uses those constants, never a literal.
- Nothing terma does writes into a repository's working tree or committed files. Its git
  hooks go under `.git/hooks`, which git neither tracks nor carries in a commit.
  The rule is about the repositories terma watches: install.sh's PATH line in the
  developer's shell startup files is outside it, even where a dotfiles repository keeps
  those files (`TERMA_NO_MODIFY_PATH` opts out), and so is the function setup defines
  there for an agent it starts through `terma launch` (`TERMA_SHELL_LAUNCHERS=0` opts
  out): like a hook entry it only calls terma, and holds no endpoint or token. What a developer collects is decided
  by `routing.Router.Route` over the profile's `routing.State` and each selected team's collection policy.
- Help text never mentions the hidden `dev` and `local` environments.
- `TERMA_POLICY_STUB`, the offline policy tests and the hook bench use, is read only by a
  build without the `release` build tag; goreleaser builds every release with it, which
  `internal/boundary` checks.
