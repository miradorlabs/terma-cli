# AGENTS.md

This file provides guidance to coding agents (Claude Code, Codex and others) when working
with code in this repository.

`terma` is a Go CLI that connects coding agents (Claude Code, Codex, GitHub Copilot, OMP,
Antigravity) to Terma and stamps the commits they produce with the agent session that
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
internal/spool/         the local queue hooks append to
internal/delivery/      sends the spool to each project, from a detached flush
internal/session/       session manifests and commit attribution
internal/doctor/        `terma doctor` checks
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
The state directory (`config.StateDir`) holds what terma writes as it runs: `relay/`,
`spool/`, `policies/`, `agents/<agent>/` (each agent's hook state), `funding/`,
`workspaces/` (session stores for work begun outside git) and `update/`; a repository's
session store is `.git/terma/`, which also keeps what a git hook in an agent's sandbox could
not write to the state directory, until a hook outside the sandbox moves it there. Names
are lowercase with dashes; `.json`, `.log` and `.lock` say what a file holds, a single
plain value has no extension, a folder of per-key files is plural, a name never repeats
its folder's, and state that belongs to one agent sits under its name. `App.Execute` resolves both directories once and passes them down;
nothing below it reads them from the environment (`internal/boundary`).

**Telemetry.** Claude Code and Codex ignore exporter settings in repository config, and
desktop apps and IDE extensions read only user-level settings. But a user-level exporter
sends everything, personal work included, under one key. So every agent's user-level
exporter points at a relay that terma runs on `127.0.0.1`. An agent whose command reads
its exporter from its environment alone (`agents.Launched`: the Copilot CLI) has no
settings file to point: setup defines a shell function (`shellrc.InstallLauncher`) that
starts it through `terma launch <agent>`, which adds the exporter to that one process's
environment, and the real command as it was once terma is gone. A hook in a collected repository
claims its session for the developer's team, naming the repository. The relay forwards
only claimed sessions, only from the processes the claim names, and only while the policy
still lists the claim's repository. It sends them with the person's sign-in, a user token,
naming the team in `X-Mirador-Project` (a profile set up with a server key sends with that
key instead), and applies the team's collection policy: agents export all content to it, and the policy alone decides what
leaves (`config.Policy.Content`). The claim also names the checkout the hook ran in, which
the relay stamps on the session's records (`stampCheckout`): its origin, branch and whether
it is a linked worktree under any policy (`vcs.*`, `terma.repository.worktree.linked`), and
its working tree, the directory in it and the worktree's name with tool content only
(`terma.repository.root`, `terma.working_directory`, `terma.repository.worktree.name`). A subagent's
claim names the session that opened it where the agent's own telemetry does not (OMP's,
`hookrun.Extension.ClaimParent`), which the relay stamps as `terma.session.parent.id`
(`stampParent`). Hook events never pass the relay, so delivery applies
the same rules when it sends them. A session marked not collected is dropped on arrival;
everything else unclaimed is held briefly and dropped: nothing unclaimed leaves the
machine. The exception is a policy in global mode, which collects every session on the
machine: it admits every repository, a hook outside git claims its session for the
policy's default project, and the relay sends what no claim places there too, except a
session an agent's hooks claim every one of (`shape.SessionKey.Claimed`): it waits for its
claim, so a hidden side thread no hook claims stays here. What is held stays in memory,
never on disk, and a stopping relay drops it.

**Skills.** For each skill a Claude Code or Codex session uses, a hook spools one
`terma.skill.definition`: the skill's name as the agent reports it, its description, and
where it is held (`hookrun.SpoolSkills`, once per skill per session). Claude Code's come
from its transcript at a turn's end: the listing it gives the model, the model's `Skill`
calls and a person's `/name`, each with the directory it loaded from. Codex's come from the
shell call that prints a `SKILL.md` under one of Codex's skill roots, and from the rollout's
record of a `$name` skill. A description leaves, never a skill's body, and a link only to a
repository the policy collects or a plugin's GitHub marketplace. A definition is content of
both kinds: hooks spool none, and delivery drops a queued one, unless the policy collects
prompts and tool content.

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
  out): like a hook entry it only calls terma, and holds no endpoint or token. What a developer collects is the
  team policy's repository list, read through `config.Policy.Admits` alone.
- Help text never mentions the hidden `dev` and `local` environments.
