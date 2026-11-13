# AGENTS.md

This file provides guidance to coding agents (Claude Code, Codex and others) when working
with code in this repository.

`terma` is a Go CLI that connects coding agents (Claude Code, Codex, GitHub Copilot) to
Terma and stamps the commits they produce with the agent session that wrote them.

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
internal/routing/       on a machine routing by teams, each profile's routing state (its
                        teams, their policies and states, the bindings) and Route
internal/runs/          the run ledger: each session's runs, and when one crossed organizations
internal/migrate/       the relay's one-way move of the machine to routing by teams
internal/account/       the platform's API (api), sign-ins (auth) and keys (keystore), their
                        secrets in the system keychain (secret)
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
setup changed: `config.json` (profiles and settings), `credentials.json`, `keys.json`, and
`setup/` (each change setup made to another tool's files, with what it replaced). The
credentials' secrets, sign-in tokens and server keys, live in the system keychain
(`internal/account/secret`), as gh keeps its token: `credentials.json` and `keys.json`
index them, and hold them only where there is no keychain or `--insecure-storage` asks.
On a machine routing by teams they are in `credentials.v2.json` and `keys.v2.json` instead,
each team's key filed by its enrollment (keychain items
`credential.v2/<profile>/<organization>` and `team-key/<enrollment>`), and
`credentials.json` and `keys.json` are left empty; `credentials.v2.json` alone marks the
machine migrated (`config.Migrated`), even once a routing state is lost. The state
directory (`config.StateDir`) holds what terma writes as it runs: `relay/`, `spool/`,
`policies/`, `routing/` (each profile's routing state), `runs/` (the run ledger),
`team-names/` (the names `terma teams` last listed), `migration-blocked.json` (why the last
relay start did not migrate), `migrating` (since when a relay has been migrating),
`migration-opted-in` (when the developer opted in to routing by teams), `agents/<agent>/` (each agent's hook state), `funding/`,
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
still lists the claim's repository. It sends them with the team's key and applies the team's
collection policy: agents export all content to it, and the policy alone decides what
leaves (`config.Policy.Content`). The claim also names the checkout the hook ran in, which
the relay stamps on the session's records (`stampCheckout`): its origin, branch and whether
it is a linked worktree under any policy (`vcs.*`, `terma.repository.worktree.linked`), and
its working tree, the directory in it and the worktree's name with tool content only
(`terma.repository.root`, `terma.working_directory`, `terma.repository.worktree.name`). Hook events never pass the relay, so delivery applies
the same rules when it sends them. A session marked not collected is dropped on arrival;
everything else unclaimed is held briefly and dropped: nothing unclaimed leaves the
machine. The exception is a policy in global mode, which collects every session on the
machine: it admits every repository, a hook outside git claims its session for the
policy's default project, and the relay sends what no claim places there too, except a
session an agent's hooks claim every one of (`shape.SessionKey.Claimed`): it waits for its
claim, so a hidden side thread no hook claims stays here. What is held stays in memory,
never on disk, and a stopping relay drops it.

**Routing by teams.** All of the above is a machine routing by its profile's one team. A
machine moves to routing by teams only once its developer opts in: `terma teams`, or `terma
setup` on a machine never set up before, records it (`migrate.OptIn`). Until then a relay
start leaves the machine as it is, and doctor says that `terma teams` turns it on;
`migrateAutomatically` (`internal/cli`) moves every machine instead. The
relay moves it to routing by teams once and for good (`internal/migrate`): only a relay
does, beside itself once it listens, holding its own lock and the spool flush's, so nothing
else sends meanwhile; the relay routes each record by the rules in force as it does, so it
moves to the new ones at the commit. It checks that the agents' hooks run its own terma, and asks whether each team
is a personal organization's with that profile's own sign-in; then it writes each profile's
routing state in a Preparing phase, copies the keys and sign-ins into `keys.v2.json` and
`credentials.v2.json`, empties `keys.json` and `credentials.json`, clears config.json's
team, and commits. A start that cannot records why for doctor (`migrate.ReadBlock`), and
the next tries again; until the commit, everything runs as before, and once it is made an
older terma finds no team, key or sign-in, and collects nothing. On a migrated machine the
profile's routing state (`routing/<profile>.json`) is the one store of the teams this
machine sends to, each selection an enrollment with its last validated policy and state,
and of the developer's bindings of owners and repositories to organizations: `terma teams`
ticks and binds, and the relay refreshes each enrollment's policy with its own
organization's sign-in. `routing.Router.Route` decides where every record goes, and a
routing state the migration has not committed sends nothing (`routing.LoadReady`). Hooks
route each location they record: a location-only placement for the session, each spooled
event with the destination Route gave it and never a project, and each session's runs in
the run ledger (`runs/`, `internal/runs`), where a run that touches two organizations'
checkouts turns Mixed and sends no telemetry until the session starts a new run. Mixed
drops whatever carries content, a record that routes by a location of its own (an
agent's turn) included; only a file event, a commit or a push, which say where something
happened and nothing else, keep their destination (`runs.Locational`). A run
touches a checkout when it runs there, or when a tool call edits, reads or searches a path
in it, or names one in a shell command (each path the command names that is there, the
nearest folder that is there above one that is not, as a new file a redirect creates,
and every `cd`'s and `pushd`'s target: `hookrun.ShellPaths`). An edit records and spools as it always has; a
read, a search or a shell command only classifies (`hookrun.Env.Classify`), in a PreToolUse
hook before it runs, so that its own result is exported after the crossing it makes: it
writes no manifest and spools nothing, and on a machine not migrated it does nothing. A hook whose own location Route drops still classifies what
the call touched, and that location, into the run ledger, and records and sends nothing
else. `runs.Touch` returns the run's locations as it wrote them, so of two hooks of one run
that each see half a crossing, the later sees it whole. The commit hooks are installed, and `prepare-commit-msg` stamps, only where Route sends the repository
to a team whose policy turns `git_hooks` on. A record's destination never changes once
routed: the relay queues each part for its enrollment, and the spool keeps each event's;
both recheck it before sending (`routing.Router.Recheck`, then `runs.MixedAt`), and send it
with that enrollment's own key from `keys.v2.json` alone, filtered by its team's content
rules.

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
  The rule is about the repositories terma watches: install.sh's PATH line in the
  developer's shell startup files is outside it, even where a dotfiles repository keeps
  those files (`TERMA_NO_MODIFY_PATH` opts out), and so is the function setup defines
  there for an agent it starts through `terma launch` (`TERMA_SHELL_LAUNCHERS=0` opts
  out): like a hook entry it only calls terma, and holds no endpoint or token. What a developer collects is the
  team policy's repository list, read through `config.Policy.Admits` alone.
- Help text never mentions the hidden `dev` and `local` environments.
