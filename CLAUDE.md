# terma-cli

Public Go CLI (`terma`) that connects coding agents to Terma and stamps the commits they
produce. Sibling of `../mirador-cli`, from which `internal/account/{auth,api}`,
`internal/config`, `internal/harness` and `internal/ui/output` were forked and rebranded
(TERMA_* env, `~/.config/terma`). Keep those packages close to their mirador
counterparts; the product-specific code is everything else.

The layout — one binary (`cmd/terma`), its command line (`internal/cli`), the hook
runtime (`internal/hooks`), the relay daemon (`internal/relay`) and the coding agents as
plugins (`internal/agents/<name>`) — is drawn in `docs/ARCHITECTURE.md`, and
`internal/boundary` turns it into tests.

## Commands

```bash
make build            # → bin/terma
make check            # gofmt -l (verifies, rewrites nothing), go vet, lint, go test ./..., plugin tests
make lint             # the golangci-lint in .golangci-lint-version, built with this Go on first use;
                      # tests are linted too, and revive enforces doc comments on exported names
make format           # go fix ./... then gofmt -w . (mirrors the .claude/hooks edit-time hooks)
make bench-hook       # prepare-commit-msg budget (<50 ms end to end), enforced in CI
make release-dry-run  # goreleaser snapshot, nothing published
make test-install-e2e # built CLI: root discovery, non-Git workspaces, ownership, uninstall
make test-install     # what CI runs: tagged goreleaser render, then install.sh (+ the cask on macOS) over loopback
go test ./internal/hooks/hookrun/ -run TestName
```

Everything the Makefile runs uses `TERMA_ENV=dev` by default. Outside it, prefix the command
yourself (`TERMA_ENV=dev go test ./...`): `terma install` and `terma setup` sign in, so a bare
run that reaches them opens a browser login on **production**. Offline install tests
must set an explicit `TERMA_POLICY_STUB`; `--harness none` alone still needs the
developer login to check the team's repository permission.

## Shape

- Agents are plugins: each coding agent is one package, `internal/agents/<name>`, with
  its hook handlers, hook-file planner, exporter configuration and relay shapes, and
  `internal/agents/builtin` is the only list of them (`builtin.Agents()`, which
  `cmd/terma` hands the command line). Every agent implements `agents.Agent` — which file
  it writes (`Plan`), which `terma hook <event>` names it handles (`Events`), which events
  flush — and only the optional capabilities it has (`internal/agents/doc.go` is the
  map), found with `Registry.With[C]()` / `Find[C](name)` and asserted with `var _` in
  the agent's package. `install`, `uninstall`, `doctor` and `terma hook` iterate the
  registry; a new agent is a new package and a line in `builtin`, never a branch in the
  core. Nothing outside `internal/agents` names an agent (`internal/boundary`). Only
  Claude Code and Codex are supported (`Registry.Support`); an unsupported agent's hooks
  are never wired by default (`install.Adapters`), whatever directory the repository
  carries or a colleague committed — only `--adapters` names one — and uninstall, doctor
  and `terma hook` still cover them.
- Hooks are thin shims; **all logic is in the binary** (`terma hook <event>`, run by
  `internal/hooks/dispatch` over the runtime in `internal/hooks/hookrun`). dispatch takes git
  hooks first without touching the registry, then the owning agent's render hook (it still
  renders under `TERMA_HOOKS=0`, returning its status as the exit code), its hooks-off
  handler or its handler; the relay start, clone wiring and flush are injected by the CLI. Never put logic in `hookmgr.ShimScript` or the husky/lefthook/
  pre-commit lines beyond "call terma, never fail, chain". Every committed entry is
  guarded (`command -v terma … || true`) so a colleague without terma sees nothing, in
  git or in their agent; the harness entries share one string, `hookmgr.HookCommand`,
  and changing it re-trusts Codex's hooks (hash-keyed) on every developer's machine.
  Hook JSON is written without HTML escaping (`marshalJSON`) — the guard carries `>`
  and `&&`.
- `prepare-commit-msg` must stay local-only and fast: no network, no git subprocess
  unless a manifest exists (then exactly one, `git diff --cached`). `internal/gitx/
  fastpath.go` locates the repo and reads `core.commentChar` from the filesystem;
  `post-commit` reads HEAD in one `git log` call. A git subprocess is ~13 ms on macOS.
- Attribution (`internal/session.Attribute`): manifests decide; the active-session
  fallback applies only when the active session has **no** manifest. Empty manifests
  are kept after `Consume` as evidence that files are tracked. Every writer of the store
  — `Touch`, `Consume`, `Prune`, and `SetActive` / `ClearActive` on the active session
  that `Touch` also refreshes — runs under one store-wide lock (`store.lock`, via
  `internal/flock`, 250 ms cap); readers take nothing, since every write is an atomic
  rename. Hooks are concurrent — Codex's `PostToolUse` is async,
  a subagent edits under its parent's session id — and unlocked, 24 concurrent touches
  kept 1 file. A test of that exclusion raises the store's `lockWait` (`patient`): the cap
  is a policy with its own test, and a loaded machine must not decide the other. Never nest it (`Prune` calls the unlocked `clearActive` for that reason),
  never create the state directory just to lock it, and a lock that cannot be had falls
  through to the write rather than dropping it.
- Spool (`internal/spool`): JSONL under the config dir, flock on append and flush.
  `delivery.lock` serializes flushers; `flush.lock` protects only local queue
  reads/rewrites, never network requests. A flush has a 30-second context deadline
  across lock waits and all sends; requests retain their 15-second deadline.
  Append/peek lock waits are capped at 250 ms. Delivery acknowledgements atomically
  remove the sent prefix and requeue held events; cancellation leaves unacknowledged
  batches for replay. Only the flusher prunes, before its initial byte snapshot.
  A `Sender` may return events as *held* (no project key yet); they re-queue at the
  tail and never loop within a pass. Delivery failure → exponential backoff 30s..1h,
  **per project**: `spool.DestinationFailed` / `RetryAt` / `DestinationDelivered`
  (`retry.json`, written only by the router inside `Flush`, under the delivery lock).
  The spool-wide window (`next_attempt`) is left for a sender that fails a whole batch;
  a `spool.PartialDelivery` never opens it, and a pass that reaches every destination
  clears one. With one window, a project whose key was refused failed every pass, the
  window grew to an hour, and hook-started flushes delivered every other project hourly.
  A project in its window is not asked (its events re-queue, counted as `Failed`, exit
  3) until it closes; `--force` (doctor) ignores windows. `spool status` lists them,
  `status` flags this project's.
  Each project goes to its key's own environment (`delivery.Router.Endpoint`, `Router.API`):
  `keys.json` files `hosts` per project with the key (`keystore.Set`/`SetFor` take a
  `keystore.Hosts`, `keystore.HostsOf(cfg)` in production; a built-in environment is
  recorded by name and resolved through the current table). A key already on file
  keeps the hosts it came with. Order: `--otlp-url`/`TERMA_OTLP_URL` (`--api-url`/
  `TERMA_API_URL`), the key's hosts, the routing record (for the API only when it names
  another built-in environment's ingest host), the profile. The map is optional — keys
  stored earlier have none and an older terma rewriting `keys.json` drops it — so a
  missing entry means "not recorded". Sending every project to the active profile's
  host had a dev-deployment project's key refused by production on every flush, and a
  hooks-only project (no routing record) had no record of its environment at all.
  A project whose send fails comes back as `spool.PartialDelivery`: what was
  delivered is acknowledged, its events re-queue like held ones, the pass carries on
  and ends as a failure. The router asks a refusing host once per pass. Before this, one
  refused project kept every other project's events queued. Doctor fails the backend
  check only on its own project's failure; another project's is a warning naming it.
  Its round-trip reads the project's own data API with the project's key when that is
  not the profile's (the signed-in credential belongs to the profile's auth host).
  Flushes are detached processes started by hooks: immediately after a commit or a
  session end and after end-of-turn capture (`stop`, `codex-stop`, `stop-failure`).
  Newly queued Claude status-line snapshots also trigger a flush because rendering
  can happen after Stop. A failed project's retry window still applies.
  Loss is accounted by reason, never in one bucket. `Expired` is age past `MaxAge`
  (14 days), `Pruned` is the 16 MiB disk bound, `Dropped` is a line that would not
  decode, and `Unroutable` (cmd) is an event no project id can route. Held events stay
  queued. `terma spool flush` exits 0 delivered, 1 failed, 2 declined by a backoff
  window, 3 ran but left work — so a script never has to parse the sentence.
  A hook swallows append errors by design, so `doctor` is the only place a spool that
  cannot be written gets said out loud: the `KeySpool` check calls `Spool.Writable`,
  which writes a probe line under the append lock and truncates it back off. Reading
  the queue is not evidence about writing it.
- Environments (`internal/config/endpoints.go`): prod is public; `dev`/`local` are
  hidden (`--env`, `TERMA_ENV`, or a profile). Never mention them in help text.
- Agent-agnostic core: an agent's file layout is known only in its own package.
  `hookrun` speaks in events (session start/end, files touched) and the tool label
  travels as data. `internal/harness` is the kit an agent's exporter is built from; its
  `Harness` interface is complete: every exporter spells out `Local`, `CurrentCredential`,
  `Backup` and `ConnectNotes`, answering "none" where it has none (Codex's `Local` is
  `(nil, false)`). There is no embeddable default and no type assertion — a capability
  asked for by assertion, or filled in by a default, silently switches off when a method
  drifts; a required method fails to compile.
- Shared file plumbing — use these, never a private copy (there were six atomic writers
  and four flocks):
  `config.WriteFileAtomic` (durable: fsync file and directory) for anything written at
  install or connect time; `config.WriteFileAtomicNoSync` for state a hook rewrites on
  every tool call (session manifests, the spool queue, and every hookrun cursor and
  checkpoint, through `hookrun.writeState`) — a file sync on macOS is `F_FULLFSYNC`,
  ~10 ms against 0.2 ms, inside an agent's turn. A cursor must never be more durable than
  the spool behind it (`Append` does not sync): after a power cut it would say "already
  sent" about an event that never reached the disk, where one that falls behind only
  replays, and stable observation ids make a replay harmless. `config.WriteJSON` for a
  state file under the config dir (private directory, indented, trailing newline).
  `internal/flock`: `Lock(ctx, path)` waits and is cancellable (it polls, 100 µs doubling to
  1 ms — at a fixed 5 ms three queued hooks took over 10 ms and a CI runner outlasted the
  session store's 250 ms cap); `TryLock(path)` takes it
  or skips, never follows a symlink, and `IsBusy` tells "held" from "failed". Lock a
  sidecar, never the file the rename replaces. **Any file that is read, edited and
  renamed back needs the lock** — the session store, `keys.json`, `codex-notify.json` and
  `statusline.json` each lost updates without it (16 concurrent writers kept 1 entry).
  `flock.Locked(path, wait, fn)` is that read-modify-write's lock: it creates the
  directory and holds `path+".lock"` around `fn` (the claim store keeps its own, since it
  writes even without the lock).
- `internal/hooks/hookmgr` by file: `hookmgr.go` is the vocabulary (`Manager`, `Detect`,
  `Change`, `Plan`, `Apply`); `git.go` terma's own shims and the shared line edits, and `husky.go`, `lefthook.go`, `precommit.go` the other three git hook managers; `user.go` the
  machine-wide hooks; `json.go` the unescaped, key-ordered JSON. Each agent's planner is
  its own package's `hooks.go`. The event-keyed hooks files (Claude, Codex, Cursor) share
  `MergeEventHooks` (`events.go`): a planner only builds its entries, and
  `HooksFile.Defaults` carries Cursor's `version` rule. Antigravity's file is keyed by
  hook name and plans itself. `ReadFile` returns an error for anything but "not there".
- `internal/hooks/hookrun` by file: `events.go` holds every spool event name and the repeated
  attribute keys and values (`TestWireNamesAreFrozen` pins the strings — add a constant,
  never rename one); `input.go` is the one bounded payload reader (`ReadInput[T]`,
  4 MiB, an oversized payload is refused by name); `lifecycle.go` the session
  start / end / files-touched steps every agent shares (`announce`, `endSession`,
  `touch`, and `Env.Open`, every session hook's first step: refuse an unsafe session id, run
  from the payload's cwd, resolve the repository); `attrs.go` the evidence header and the
  bounded-string guard; `evidence.go` the bounded funding-evidence reader agents capture with; `state.go` the
  state directory names and timing constants. A handler resolves the repository once
  and passes the `*repo` down. Only Codex patches and Cursor's `subagentStop` dedupe and
  sort their `files` list (`hookrun.UniqueSorted`, at the call site); every other agent keeps
  the reported order.
- doctor: `terma on PATH` warns when a *different build* of terma (by content hash — it
  never runs what it finds) sits elsewhere on PATH or in `doctor.WellKnownBinDirs`: an app started
  from the Dock gets the system PATH, so `/usr/local/bin/terma` is what Cursor's hooks run,
  and a build from before `.terma/settings.json` does not see the binding. The test app
  blanks them (`App.binDirs`). When no terma is on PATH at all (a `make build` run as bin/terma),
  or another build is ahead of this one, the fix is the one quoted command that puts this
  build's directory on PATH in the developer's shell (`doctor.AddToPathCommand`: the PATH line
  appended to the startup file and sourced; `fish_add_path` for fish). It lands after
  whatever else the startup file holds. The hooks check is two: `commit hooks installed` (git wiring, all or
  nothing) and `agent hooks run` (`doctor.AgentHooksCheck`, shared with status — a fraction,
  `Check.Ready`/`Of`). A commit is stamped with the session that touched its files and a
  session exists only because its agent's hooks announced it. The readiness checklist
  names agents awaiting trust instead of assigning speculative spend percentages.
  Doctor also compares PATH's executable with the running build; a mismatch can leave
  scratch events without a project binding. Only the developer's own agents
  (`config.Profile.Harnesses`; empty = all) count — a colleague's committed Codex hooks are not
  theirs to trust. The scratch commit's checkout, commit and removal run under
  `scratchGitTimeout` (2 min) through `gitx.GitWithin`, never a hook's 2-second
  `gitx.Timeout`: a 2.7 GB checkout takes 11 s and was killed on every run, each
  killed `worktree add` leaving a registration locked "initializing" that `prune`
  skips. Removal forces twice for that lock, and each run first clears its own
  abandoned ones (`<tmp>/terma-doctor-*/wt`, directory gone) — nothing else.
- The command surface is small on purpose (`internal/cli/command_surface_test.go`). `terma --help`
  lists `primaryCommands` — setup, install, status, doctor, session, usage, org,
  uninstall, update — and everything else is `Hidden: true`, **not removed**: login/logout/
  whoami, connect/disconnect/telemetry/harness, project, principal, config, spool, version,
  hook, agent — and cobra's `completion`, hidden (`CompletionOptions.HiddenDefaultCmd`), not
  removed: the Homebrew cask's `generate_completions_from_executable` runs `terma
  completion <shell>` during `brew install`, and a failing command fails the install.
  `blame` was removed outright (2026-09-28, a product call: not part of terma for now),
  and `desktop` and `shim` with the code behind them (`removedCommands`) — no message may
  name one. doctor's round-trip still reads
  `terma.commit` back through `api.CommitLog` (`commitLogWindow`). Hidden commands are what automation and CI run and what terma's own fix-it
  hints name, so they must keep working; `project` is advanced because `install` binds a
  repository to its project and the selection only scopes the read commands elsewhere. A new
  command is advanced unless a developer needs it day to day — an unclassified or un-hidden
  one fails the test. `TestEveryCommandAMessageNamesExists` reads every backtick-quoted
  `terma <name> …` in the shipped source (hints, errors, help, comments) — and the two
  unquoted shapes, a doctor `Fix: "terma …"` and a sentence ending `with: terma …` — and
  requires the command to exist: `terma login` recommended the removed `terma use` for
  weeks because nothing checked, `terma connect` recommended the fork's `terma trace list`
  for as long as its hint stayed unquoted, and the same test stops a cleanup from deleting
  a command a hint names. Quote a command in backticks when a message names one.
- `terma setup` is the machine half (`internal/setup`): it signs in, records the developer's agents
  (`config.Profile.Harnesses`), selects a team, and fetches its policy with the
  developer's login token (`api.CollectionPolicy`, GET `/v1/policy?project_id=<team>`
  on the auth host). Reading policy never creates a server key. Cached team policies
  live under `policies/`; the selected team's machine-wide coverage also lives in
  `config.Profile.Policy`. The relay refreshes them every minute; hooks stay local.
  Failed fetches retain each team's last validated policy; an unfetched team cannot
  export. A policy from another team, organization, or environment grants nothing.
  A temporary rejection of developer tokens must not trigger permissive defaults or
  a server-key workaround. Routing records can only narrow signals and content.
  Both the native outbox and hook spool enforce policy again at delivery. Path
  exclusions withhold named excluded paths and free text whose source files cannot
  be established.
- The platform (terma-frontend) commits a repository's binding and agent hooks, so `terma
  install` is optional. What a commit cannot do is done on first use: the relay mints a
  claimed project's missing key with the signed-in credential (`daemon.KeyMinter`, stored as
  the project's key, 10-minute backoff after a failure), and the first claiming hook in a
  clone whose binding uses terma's own shims sets `core.hooksPath` (`wireCloneOnFirstUse`;
  one stat afterwards — the hook-restoration record says it was wired). internal/cli's `TestMain`
  gives every test a private HOME: a setup test that sandboxed only terma's config dir
  rewrote the developer's real `~/.codex/config.toml`.
- **Global mode** (`config.ModeGlobal`, the organization's policy): company laptops where
  the organization wants all AI spend — invasive on purpose, never the default. What
  changes, all of it written by `terma setup` and removed by a setup back in repo mode:
  - Placement: all native exports and hook events go to the selected team's
    `DefaultProjectID`. A directory outside any repository gets the private workspace
    store (`hookrun.Env.Policy`, `repo()`). The relay forwards all native exports
    immediately to the selected team's project under its capture rules, without
    requiring a session or a process claim (`relay.Options.CatchAll`, marked
    `terma.relay.attribution=catch-all`). A switch back to repository coverage also
    withholds queued catch-all exports.
  - Machine-wide agent hooks (`internal/globalmode`): terma's entries in Claude Code's,
    Codex's and Cursor's user-level hooks files, `terma hook --user <event>` by absolute
    path (`hookmgr.UserHookCommand`, recognized by shape). For the agents they cover they
    are the ones that act — a repository's committed hooks step aside (`globalmode.Machine.Yields`, from
    `relay/user-hooks.json`) because Codex skips repository hooks until trusted; outside
    global mode a `--user` hook does nothing. Codex asks the developer to trust them once.
  - Managed configuration (`terma setup --managed-config <dir>`): the same hooks as
    Claude Code's `managed-settings.json` and Codex's `requirements.toml` `[hooks]`, calling
    terma through `$HOME` (`hookmgr.ManagedHookCommand`), which Codex runs with no trust
    step. Where they are deployed (`Machine.ManagedDeployed`) setup writes none of its own.
  - Commits (`internal/globalmode/git.go`): `git config --global core.hooksPath` → a directory with
    a script per git hook name: terma for prepare-commit-msg / post-commit, then the hook
    git ran before (the repository's `.git/hooks`, or the developer's own global directory,
    recorded in `.previous-hooks-path` and restored). A repository's local core.hooksPath
    (husky, `terma install`'s shims) outranks it.
  - Tests: `test/live/global_test.go` runs it with real agents (safe anywhere: the sandbox owns
    HOME and git's global config); `make machines` (in `test/live/`) runs it, and the core relay
    contracts, on a fresh Linux machine in Docker (`test/live/machine/Dockerfile`, seccomp
    unconfined for Codex's bwrap), including the managed-config scenario that writes
    `/etc` (`TERMA_MACHINE=1` only).
- Everything else per-repository is `terma install`, including what `setup` used to do on the way: per-clone `core.hooksPath`
  wiring, the Claude status-line wrap, and the **spool key**. That key (`keystore.Get(project)`)
  is what hook events are delivered with; pointing a telemetry agent stores it as a side effect
  (`keystore.SetFor`) and nothing else did, so a developer whose agents are all hooks-only
  (Cursor, Antigravity) had every event held forever. `ensureSpoolKey` mints one when hooks are
  wired and none is stored. Every real install checks collection policy with a developer
  login before writing a binding, including hooks-only agents and `--harness none`;
  offline fixtures explicitly set `TERMA_POLICY_STUB`. Fix-it hints follow the same
  split: sign-in → `terma setup`; a missing key, an unwired clone, an agent on the wrong
  project → `terma install`. A server key (`TERMA_API_KEY`) identifies its own project, read from
  the API gateway's `/v1/identity` (`serverKeyBinding`): the account service's
  `/v1/projects` accepts only a signed-in user, and a `--project` or existing binding
  naming another project is refused, since the key could not deliver its events. A server
  key cannot read collection policy: real installs use the saved developer login for
  policy even when `TERMA_API_KEY` identifies and delivers for the binding. Without a
  saved login, unset `TERMA_API_KEY` and run `terma setup` first.
- Per-repo routing is the local relay (below, and `docs/RELAY.md`): `terma install`
  points each of the developer's agents' user-level exporters at the relay
  (`connectMachineRelay`, shared with `terma relay setup`), keeps the project's key in the
  keystore and its policy in the routing record (`internal/routing`: `routing/<id>.json`,
  signals, prompts, tool content — what the relay enforces for the project), and starts
  the relay. Hooks in the repository claim its sessions; nothing else leaves the machine.
  There are no PATH shims, wrappers or startup-file edits: the pre-relay shims and
  `terma shim` were removed before release, and nothing cleans up after them (a dev
  machine that ran one deletes `~/.config/terma/shim` and the marked PATH block itself).
  Codex's CLI TUI therefore runs in Codex's daemon when one runs (no `-c` overrides force
  it in-process).
- Codex is split across two scopes and neither is optional. Telemetry supports user-level and runtime configuration: Codex strips `otel` (with `notify`, `profile`, `profiles` and the provider keys)
  out of a project's `.codex/config.toml` and warns at startup, so `--scope local` has
  nothing to write. `terma connect codex` and `terma install` write user-level
  `config.toml` (install's points at the local relay). The repository half is hooks
  (`internal/agents/codex/hooks.go`): `SessionStart`, `PostToolUse`, `Stop` and `SessionEnd` in
  `.codex/hooks.json`, which Codex loads only for a trusted project *and* only after the
  developer trusts each entry from inside Codex. Until then the file is inert and
  nothing says so, which is why `doctor` reads the `[hooks.state]` record in the user's
  config (`hookTrustFor` in internal/agents/codex) and reports untrusted hooks as a warning. Terma
  never writes that table.
- Codex never exports what it **said**: its OTel events carry `prompt` and tool
  `arguments`/`output`, `response.completed` is token counts, and no switch adds the reply
  (every record of a live 0.155.1 session, 2026-09-19). So `codex-stop` (and `codex-notify`,
  `codex-session-end`; one locked cursor per session under `reply-cursors/`) reads the
  turn's assistant messages from the rollout (`readRolloutReplies` (internal/agents/codex), the same confined
  open as funding, 32 messages / 1 MiB per invocation, 16 KiB per message) and spools
  `terma.assistant.message`. It is the **one** place terma reads conversation content, and
  it is gated on the consent prompts travel under (`repliesConsented`): the repo's
  routing record and the machine-wide Codex config must each allow prompts where they
  exist — a hook cannot tell which started the session — and with neither, nothing is read.
  It fails closed: a source that exists and cannot be read (a half-written routing record, a
  `config.toml` that does not parse) might be the one that withholds prompts, so any load
  *error* is a no; a missing file is not an error and simply is not a source.
  Never widen it: not the user's prompts (Codex sends those), not tool output, not
  `last_assistant_message` from a hook payload. The event is stamped with the rollout's
  timestamp, not the hook's clock, or every interim message would sort after the tool calls
  it introduced. `message_id` is Codex's own (`msg_…`). The turn the platform knows is the
  OTLP **trace id** (`task_started.trace_id`), sent as `trace_id`; the rollout's `turn_id`
  UUID matches nothing native and is informational.
- Codex never exports the thread's **name** either: a hidden side conversation (its own
  `conversation.id`, a fixed "Generate a concise, single-line task title…" prompt, no link
  back) generates it, and it lands only in `$CODEX_HOME/session_index.jsonl`, keyed by the
  thread it names (0.157.1, 2026-09-27). The same three hooks read the latest line for their
  session (`readThreadTitle` (internal/agents/codex)) and spool `terma.session.title` (`title`, stamped
  with the index's `updated_at`) when it is new or renamed; `codex-titles/` keeps the last
  `updated_at` sent. The name restates the first prompt, so it is the one other read of
  conversation content and travels under the same `repliesConsented` gate.
- Codex names no edited file: `PostToolUse` carries the tool call, and the paths live in
  the apply_patch envelope inside `tool_input.command` (`applyPatchPaths` in internal/agents/codex). That
  hook is `async` in the committed file because it fires on every tool call and nothing
  terma returns can change what Codex does; `SessionEnd` asks for Codex's maximum
  3-second timeout and fires late — on close, or 30 minutes idle — so it only clears
  state, and `ActiveTTL` ages out sessions that never end. `Stop` is synchronous
  with a 3-second timeout: it must finish bounded local rollout capture before
  `codex exec` exits. Its network flush is detached.
- Export reach (`internal/harness/scope.go`, `Reach`): a *global* connect either switches
  the exporters on for every session (`--exports everywhere`, the default and the only
  behaviour before) or leaves every one of them off so that repositories decide
  (`--exports repos`). The narrow half is the same layering `--scope local` uses, from
  the other side: the user file keeps what only it can hold — endpoint, key, and
  Claude's `CLAUDE_CODE_ENABLE_TELEMETRY` master switch — and a repository's
  committed policy switches its signals back on. Nothing stores the choice: connected,
  pointed at Terma, and exporting no signal *is* `ReachRepos`, because a repository
  policy is the only thing that can make it send, and `status`/`doctor` read it back
  that way. Codex cannot be narrowed (one config file, no project otel), so setup
  connects it everywhere and says so rather than silencing it.
- install's output (`internal/cli/install_ui.go`, `installUI`): one marked line per step (`ok`, or
  `warn` for one that needs the developer), a verdict, then numbered next steps (`then`) —
  the reload, the files to commit, a declined PATH line, Codex Desktop approval, doctor's
  fixes. Everything long-form (the plan's file list, policies written, git wiring, doctor's
  per-check lines) goes to `ui.detail`, which is stdout under `--verbose` or `--dry-run`
  and discarded otherwise. New install output goes through one of those, never straight
  to `cmd.OutOrStdout()`. Doctor runs behind a spinner as the `Verified` step and leaves
  out the routing warning when a next step already says to reload the shell (install's
  own process always predates the PATH block). The first install under a newer release
  also does the machine half of `update --refresh` (`refresh.Refresher.Machine`, gated on
  `selfupdate.NeedsRefresh`, so source builds and tests never touch home files) before
  verifying, and records it; the repository half is its own hook plan, which rewrites a
  stale committed file as it adds a missing one.
- Tool content follows the same rule (`contentChoices` in `internal/cli/install.go`): `--exclude-tool-content`
  when given, else the last choice for the project; a bare re-install used to switch it
  back on.
- Prompt capture (`contentChoices`): `--prompts on|off` (`--exclude-prompts` is the older,
  hidden spelling); otherwise install never asks: it keeps this developer's last choice for
  the project (`routing.Record.IncludePrompts`), on for a first install, and its Prompts line
  names the `terma install --prompts off|on` that changes it. It lands in the routing record
  and a newly written repository policy; only an explicit `--prompts` rewrites an existing
  committed policy (`updatePolicy`). A bare re-install used to switch prompts back on.
- `terma install` writes the repository half of that arrangement by default into the
  same committed `.claude/settings.json` the hooks live in, after the hook plan applies
  so both merges land in order. Re-running install preserves an existing policy unless
  export flags explicitly change it. Per-repo routing is configured independently.
  A pre-existing OTLP conflict there is reported and
  skipped, not fatal — the hooks and the binding are already written by that point.
- The connect workflows are `internal/connect`: `Global` (machine-wide) and `Local` (a
  repository's committed policy) share one gate — report conflicts, refuse what terma
  cannot clear, refuse what needs `--force`, ask — and resolving, storing and asking for a
  key come in as `connect.Steps`, so the package imports no account or spool code.
- Connect scope (`internal/harness/scope.go`): Claude Code's exporter is global; its `.Local(root)`
  writes `<root>/.claude/settings.json` and renders only `claudeLocalKeys` (internal/agents/claude) — the three
  exporters, the four capture switches, the traces beta flag — never the endpoint, key
  or master switch. A local file is a policy, not a connection: its
  `Status().Connected` is false, and `status`/`doctor` judge connectedness from the
  global file alone. `claudeLayer` says what outranks each file; a conflict in the file
  being written is clearable, one above it is not. Codex has no local scope.
- Claude Code never gets `OTEL_RESOURCE_ATTRIBUTES`: it is the
  user's variable, the key names the project, and Claude Code stamps `user.id`,
  `user.email` and `service.name=claude-code` itself. The project a configuration
  reports to lives in the connect journal (`journal.ProjectID`, from
  `Exporter.ProjectID`). User-level settings require a journal for removal; committed
  repository policies can be removed in any clone, but without the journal only a key
  holding a value Terma writes (`renderedByTerma`: `otlp`/`none`, `0`/`1`) — a developer's
  own `OTEL_LOGS_EXPORTER=console` stays.
  `enduser.id` / `mirador.project.id` resource attributes are Codex and OpenCode only.
- Sessions (`internal/account/auth/store.go`): `credentials.json` holds, per profile, one
  credential per organization plus which is active (`{active, organizations}`).
  `cmd.signIn` is the only way a command obtains a credential: verify the stored one
  (`/v1/whoami`, which also refreshes it), reuse it, and open the browser only when
  the organization has none. `SaveCredential` activates and reports the session it
  displaced (revoked best-effort); `UpdateCredential` is the refresh path and never
  changes which organization is active. `logout` revokes every stored session.
- `terma org use` (`internal/cli/org.go`) changes account scope only. `terma install` selects
  and saves projects per repository. An organization with one project is bound to it
  without asking (`soleOrPick`). With several, on a terminal it asks every time, the bound
  project marked and kept by Enter; without one, or with `--yes`, it keeps the binding. When
  install signs in, `resolveBinding` checks the binding against the projects that
  credential lists: one made in another environment or organization is named and
  replaced, never used — used as-is, its first key mint was refused with the gateway's
  "run `mirador project list`". An offline policy fixture without a credential keeps it unchecked, and a
  kept binding keeps the environment it recorded. Project-scoped
  reads resolve the Git worktree's `.terma/settings.json`, unless `--project` or
  `TERMA_PROJECT_ID` explicitly overrides it. Machine profiles have no project defaults.
  Keys are remembered per harness per project in `keys.json`
  (`keystore.SetFor`, written by every connect) so returning to a project reuses the
  harness's own key; `resolveKey` checks the harness config, then the keystore, then mints.
- Linked worktrees (`project.Resolve` / `ResolveDir`, `gitx.LinkedWorktreeFS`): every
  reader of a binding — hooks, the status line, routing, status, doctor, install, refresh
  and the OpenCode plugin's `readProjectID` — takes the checkout's own binding, else, in a
  linked worktree, its main checkout's. The binding is often gitignored and `git worktree
  add` does not copy ignored files; before this, every event from such a worktree had no
  project and was dropped as `Unroutable` at the next flush, in silence. It follows git's
  `commondir` link, never directory nesting (a nested separate repository still inherits
  nothing). A hook reads the git directory's `commondir` (one small file, absent in a main
  checkout) on every run, to name the worktree, and the main checkout's binding only when
  the checkout has none; no git subprocess. A bare repository's worktrees have no main
  checkout to fall back to. Events from a linked
  worktree report the **main** checkout's directory name as `repo` and git's name for the
  worktree as `worktree` (`AttrWorktree`), so one repository's worktrees — Claude Code's
  `.claude/worktrees/agent-…` among them — group as that repository. `install` and
  `refresh` read through it but write the checkout's own files; `uninstall` reads only the
  checkout's own binding, so it never acts on the main checkout's.
- Terminal courtesies (`internal/ui/style`, `internal/ui/spinner`): colour and the
  four-square spinner draw only on a terminal a person is watching — never in a
  buffer, a pipe, an agent (`CLAUDECODE` and friends), `NO_COLOR` or `TERM=dumb` —
  so tests compare plain strings. `doctor` streams each check as it finishes
  (`doctor.Progress`) and polls the round-trip every second. A command a message tells
  the reader to run is quoted in backticks, and on a terminal it is drawn as one
  (`Palette.Command`, bold brand purple) with the backticks dropped, so a copy is the
  command alone: `Palette.Commands` for a string, `style.Highlight(w)` for a writer
  (status, refresh, the top-level error line), doctor's `fixText` for a fix that leads
  with a bare `terma …`. Plain output keeps the backticks.
- Interactive prompts (`internal/ui/prompt`): a pure form model plus a raw-mode driver on
  `x/term`, no TUI dependency. Shown only when `canPrompt()` (stdin/stdout/stderr are
  terminals, no agent env var); every box has a flag and `--yes` skips the form. The
  pickers (`internal/cli/pick.go`: the install project picker, `terma org use`) are a `Choice`
  form through `prompt.Choose` — arrow keys, Enter picks, Esc is `errCancelled`, the
  cursor starting on the default or current row; the numbered list answered by number
  or name is the fallback when stdout is a terminal and stdin is not.
- OpenCode (`internal/agents/opencode/harness.go`; plugin `internal/agents/opencode/plugin/terma.js`,
  embedded): the harness is a dependency-free plugin written whole into
  `~/.config/opencode/plugins/terma.js` with one `const CONFIG = {...}` line spliced in
  (the raw template ships `CONFIG = null` and is inert). The user's `opencode.json` is
  never touched; the key stays in the headers-helper script. `bun test
  internal/agents/opencode/plugin` drives the plugin the way OpenCode would (`make check` runs
  it when bun is present). Local scope is `<root>/.opencode/terma.json`, policy only.
- The `project` package is imported as `termaproject` in `internal/cli` because the
  command line has a ported `type project struct`.
- Read commands (`usage`, `session`, `principal`; `internal/cli/usage.go`, `internal/cli/session.go`,
  `internal/cli/principal.go`) are thin over the API gateway's `/v1/ai/*` and
  `/v1/metrics/query` (`internal/account/api`: `ai*.go`, `metrics.go`). `usage` is PromQL
  `increase()` over the `terma.ai.*` counters — the same idiom as the terma-frontend
  insights page — so it measures spend *inside* the window; `session list` selects by
  a session's **last activity** (`--since` → the gateway's `active_after`; the gateway has
  no upper bound, so `--until` is applied client-side to `last_activity_at` over a page
  walk). A session is `session_id` + `source_system`; the gateway never exposes a routing
  key, pages the list by offset (`page`/`per_page`), pages events by cursor, and serves
  one session's roll-up only as the `summary/stream` feed, which `session get` reads one
  frame of under a 15-second bound. Names never travel on the wire: `--user`/`--api-key` resolve
  through `/v1/ai/principals` (`principalIndex`), and a substring that lands on two
  people is an error, never a guess. Semantics for agents live in `docs/INSIGHTS.md`.

## Funding attribution (in progress)

- Claude SessionStart/Stop capture allowlisted account state and credential-presence
  hints as `terma.session.account`; StopFailure adds a typed `terma.session.limit`.
  Codex's user-level notify (installed by a machine-wide `connect codex`; a repository routed by
  `terma install` keeps the developer's own notifier and relies on the hooks) and its
  repository Stop/PostToolUse/SessionEnd hooks incrementally drain confined rollout records into `terma.session.quota`,
  preserving source order, turn IDs, provider timestamps, missing values and decimal credit balances.
  A pre-existing Codex notifier is recorded in `~/.config/terma/codex-notify.json`, run
  after capture, and restored by `disconnect codex`. The file holds one chain per Codex
  config path (`chains`), so a second `CODEX_HOME` never overwrites the first's notifier;
  every change goes through `updateCodexNotifyRecord` under a lock, because it is one file for every config on the
  machine. The notify edit writes where
  `tomlFile` writes: through a symlinked `config.toml`, keeping a mode tighter than 0600.
  Durable cursors advance after spooling; observation IDs allow replay deduplication.
  Backlog/read status uses `terma.session.capture`, separate from provider quota.
  Native OTel remains the usage counter; these events are funding evidence only.
  See `docs/FUNDING-INSTRUMENTATION.md` for schemas, limits and delivery semantics.

- Status line (`internal/agents/claude/statusline_hook.go`, `internal/agents/claude/statusline.go`):
  `terma install` (when Claude Code is one of the developer's agents; `--no-statusline` opts out)
  and a global `connect claude` put `terma hook statusline` in front of the user's
  `statusLine.command` and records what it replaced in `~/.config/terma/statusline.json`.
  The hook spools `terma.session.quota` (the plan's `rate_limits` windows, `fast_mode`,
  `prompt_id`, the running estimate) when the snapshot changes (including prompt, reset and cost) or every 10 minutes, and
  runs the previous command through `sh -c` with the same bytes, environment and
  directory, output connected straight through, exit status returned, cancellation
  forwarded to its process group on Unix. Other platforms cancel the shell and
  bound the wait for output pipes; descendant termination is not guaranteed.
  Terma also times out the renderer after 30 seconds (positive duration override:
  `TERMA_STATUSLINE_TIMEOUT`), returning 124. Capture starts detached delivery before
  waiting for the renderer. Pipe draining is bounded to 100 ms after shell exit or
  cancellation, including on Unix.
  Only `command` changes: `padding`, `refreshInterval`,
  `hideVimModeIndicator` and unknown options are copied. The installed string falls
  back to the previous command when `terma` is not on the PATH. A user who later
  replaces the entry wins; `status`/`doctor` say capture stopped. Never written into a
  repository's `.claude/settings.json` (it would override every colleague's own).
  `TERMA_HOOKS=0` still renders, it only stops capturing. The first rendered line is
  prefixed with a brand-coloured `t` (`style.BrandSequence`, honours `NO_COLOR`) while
  capturing, so the mark means "watching", not "installed". No previous renderer
  means silent capture, and an empty renderer output stays empty. Preserve original
  stdout bytes after the prefix, stderr and exit status; cancel the entire renderer
  process group. Compatibility checks live in `docs/STATUSLINE-COMPATIBILITY.md`. Live-verified 2026-09-15: a
  Team seat gets `rate_limits` too, not only Pro/Max as documented.
- Antigravity CLI (`agy`, Google's Gemini CLI successor; `internal/agents/antigravity/hooks.go`,
  `internal/agents/antigravity/handlers.go`, `internal/agents/antigravity/harness.go`): hooks only, no
  OTLP export. `.agents/hooks.json` is keyed by hook *name*; terma owns the `terma` key
  whole (other names untouched, a developer's `"enabled": false` preserved) with
  `PreInvocation` / `PostToolUse` (unmatched) / `PostInvocation` / `Stop`, never
  `PreToolUse` (it demands a decision). Payloads are protojson camelCase; the session key
  is `conversationId` (also `ANTIGRAVITY_CONVERSATION_ID` in the hook's env); the
  repository is `workspacePaths[0]`, and the hook's cwd is `<repo>/.agents`. Edit paths
  come from `toolCall.args.TargetFile` (agy's own tools) or the vendor-style keys. Every
  handler prints `{}` to stdout — agy's documented reply — so `hookrun.Env.Stdout` is now
  set for all hooks. agy loads hooks only in a **trusted** workspace (`trustedWorkspaces`
  in `~/.gemini/antigravity-cli/settings.json`, read by `trustsWorkspace` in internal/agents/antigravity) and only
  when the repository is the conversation's workspace: `agy -p` from an unregistered
  directory uses its default CLI project's scratch folder and loads no repository hooks
  (`--add-dir <repo>` binds it). Payloads and transcripts carry no token counts; the
  observations are activity evidence with every `*_status` unavailable. Observation
  capture is shared with Cursor (`hookrun/observation.go`, per-tool state dirs).
  Every `PostToolUse` step is a `terma.tool.call` (`tool_call_id` = `step-<stepIdx>`,
  agy's own ever-growing step index; no duration; `args` opened only for an edit's path),
  and an edit's `terma.files.touched` carries the same ids — it is what the call changed,
  not a second call. agy names no turn: `invocationNum` restarts each turn,
  `initialNumSteps` moves each *invocation*, and Stop's `executionNum` is per process (0
  on both turns of a resumed conversation), so `turn_id` is `turn-<initialNumSteps at
  invocation 0>`, recorded by `PreInvocation` (`internal/agents/antigravity/turn.go`,
  `antigravity-turns/`) and read back by the turn's other hooks.
  Live-verified on agy 1.2.4, 2026-09-16, and 1.2.7, 2026-09-18; see
  `docs/ANTIGRAVITY-INSTRUMENTATION.md`.
- Cursor IDE/CLI hooks capture `terma.session.observation` with conversation/generation
  IDs and durable local sequence. Response/Stop tokens are optional snapshots, never
  additive counters. Context occupancy is not billing quota; plan and funding stay
  unavailable. Checkpoints preserve pending spool appends and stable replay IDs.
  See `docs/CURSOR-INSTRUMENTATION.md`; account email is hook-supplied, no auth files
  or transcripts are read. Response/Stop trigger detached flushes; stop has
  `loop_limit: null` so observations continue beyond five follow-up loops.
  Tool calls (`internal/agents/cursor/tool.go`): `postToolUse` / `postToolUseFailure`
  spool `terma.tool.call` — `tool_name`, `tool_call_id` (Cursor's `tool_use_id`),
  `duration_ms`, `status`, `failure_type`, `is_interrupt` — bypassing the observation
  checkpoint because the native id is the replay key. `tool_input`, `tool_output`,
  `error_message` and `cwd` are never read; a call attributes no file and starts no
  flush. The generic pair is the only one wired: Cursor's `before*` gates sit on every
  call's critical path and fail open only by default (a schema-mismatched reply or
  `failClosed` blocks), and `afterShellExecution` / `afterMCPExecution` restate the
  same calls without a `tool_use_id` and with their output.
- Subagents (`internal/hooks/hookrun/subagent.go`, `docs/SUBAGENT-INSTRUMENTATION.md`) have two
  shapes and the events keep them apart. Claude Code and Codex run a subagent *inside* the
  session: the hook payload keeps the parent's `session_id` and adds `agent_id` /
  `agent_type`, so terma spools `terma.subagent.start` / `terma.subagent.end` under the
  parent and stamps the same agent facet on everything the subagent's hooks produce.
  `agent_id` is the discriminator (`agentAttrs`): `claude --agent <name>` sends
  `agent_type` on every hook of the session, so a type without an id stamps nothing.
  Manifests and trailers stay per session.
  **Claude Code** (live-verified 2.1.278, 2026-09-21): `SubagentStart` names no model and
  `SubagentStop` no usage. The parent's `PostToolUse` for the `Agent` tool (`Task` before)
  has both, which is why the committed matcher is `Edit|Write|MultiEdit|NotebookEdit|Agent|Task`
  and `PostToolUse` branches on the tool name before treating a payload as an edit.
  `terma.subagent.call` carries `agent_id`, `model` (`resolvedModel`), `tool_call_id`,
  `status` and — only when the parent waited for it — how the run went: duration,
  tool-use count, tool stats, `final_context_tokens`. It is an event of its own because it
  arrives *after* `SubagentStop`, from a separate hook process. **It says nothing about
  what the run spent, and no hook does**: the response's `usage` is the subagent's *last
  API request* and `totalTokens` is that request's classes added up — the context's final
  size (two live runs: requests 10/0/13071 then 8/13071/2357, `usage` = the second). The
  first cut of this event sent them as a run's roll-up; a review caught it before release.
  `totalTokens` travels as
  `final_context_tokens`; the by-class numbers are not sent (the native export has that
  request under its own id) and a test keeps them out. The response also holds
  the task's `description`, its `prompt` and the reply `content`; `claudeAgentResult` has
  no field for them, and a test plants sentinels in all three.
  **Codex**: a subagent is a thread the session spawned. Its hooks carry the root's
  `session_id`, the child thread's id as `agent_id`, and the child's own rollout as
  `transcript_path` (`hook_runtime.rs`, read 2026-09-17; not seen live). `SubagentStart`
  reads that rollout's first line for the spawn record (`rolloutSpawn` (internal/agents/codex) →
  `agent_parent_id`, `agent_depth`, `agent_nickname`, `agent_path`, `rollout_status`);
  `SubagentStop` is per child *turn*, not a bracket. Capture inside the thread must read
  the child's rollout — `codexRolloutID` takes the thread from the rollout file's own
  name — or the confined open refuses it as another thread's and every quota and reply
  there is lost. Codex's source fires no `SessionStart` for a spawned thread; the one-line
  read there stays in case a build does.
  **Cursor** and **OpenCode** give the child a conversation or session of its own.
  OpenCode's `terma.session.start` carries `parent_session_id`, its spans
  `opencode.parent_session.id`, its OpenRouter requests `trace.parent_session_id` and
  `trace.agent` (the plugin's `chat.params` hook; OpenRouter's broadcast is otherwise
  blind to the link), and the child is never made active — it would claim the
  developer's next hand-written commit. Cursor's `subagentStop` is filed under
  `parent_conversation_id`, names the subagent as `agent_id`, and folds a manifest the
  subagent built under its own conversation id into the parent's (`session.Store.Merge`),
  so the commit is stamped once. Cursor's `subagentStart` is never wired — a hook printing
  nothing blocks the spawn, and the committed guard prints nothing without terma.
  **Antigravity** names no subagent in its payloads.
  No transcript, task text, description or last message is read.
  Codex trusts hooks entry by entry: a developer who trusted terma's before the two
  subagent entries existed has a file that counts as trusted and two hooks Codex skips in
  silence, so the adapter compares entries (`codex.TermaEntries`,
  `hookTrust.TrustedKeys`) and doctor names what is skipped.
- `docs/collection-matrix.html` is the harness × information × mechanism matrix (open it
  in a browser). Update a cell when a mechanism ships or a live check changes it.
- `test/live/` runs the real harness binaries with real credentials through the real `terma`
  binary and checks the matrix's promises across the hook spool, an in-test OTLP receiver
  and a pseudo-terminal (`make live` there; own Go module, built in Docker, run natively).
  Interactive sessions go through a pty because the status line and the trust dialog only
  exist there; `golden/` holds each surface's attribute names so drift fails a test. A
  scenario without its credential is reported as not run, never as a pass.

## Local relay

- `docs/RELAY.md`. `terma relay setup|run|status` (hidden): the agents' global
  exporters send to `127.0.0.1:43180` with a local token (`relay/token`); only sessions a
  hook in a bound repository claimed (`relay/claims/<session>.json`, `internal/relay/claim`,
  no OTLP dependency — every hook imports it) are forwarded, per project, with that
  project's key and host (`delivery.Router.Endpoint`), content filtered by its routing record.
  Unclaimed records are held 2 minutes in memory, then dropped; nothing unclaimed touches
  disk. A part that may leave is written — content policy applied, and applied again at
  delivery (`withholdQueued`: a project that turned prompts off since sends none of the
  prompts still queued) — to its route's outbox
  (`relay/outbox/<project>/<tool>/`, `internal/relay/outbox.go`) before the export is
  answered, and one sender per route delivers it (`forward.go`: merged requests, jittered
  backoff 1 s → 2 min honouring Retry-After, a refused key retried 5 min → 1 h and never
  dropped, other 4xx to `.dead/`, OTLP partial success counted); a restart delivers what
  the last relay left (`recovered_from_outbox`). Bounds: 256 MiB / 14 days, `.dead/` 32 MiB. Claims come from `emitFor` and, for hooks that spool nothing, from the payload
  (`hookrun.ClaimFromPayload`, fed a bounded copy of stdin in `internal/cli/hook.go`); hooks write
  none unless `relay/token` exists. `relay setup` and the claiming hook start the relay
  (`daemon.Spawn`, single instance on `relay/relay.lock`, a minute's backoff after a failed
  start recorded in `relay/last-error`); it idles 8 hours, because Codex emits
  `conversation_starts` before any hook and never retries it.
- A claim is scoped to processes, not just a session: it carries the hook's ancestors
  (`internal/procinfo`), and the relay forwards a record only from a process the claim
  names (`procinfo.FindSender` per connection, retried at the connection's next exports if
  it failed; an unresolved sender is covered only by a claim that names no processes —
  a platform where hooks cannot read them — and never widens one that does: counted
  `sender_unresolved`, held and dropped). Claude keeps a session id across `--resume` in any directory,
  so a session-only claim forwarded personal work. A Codex subagent's `agent_id` is its
  own thread and is claimed too. OTLP/JSON trace and span ids are hex and must be
  converted before protojson (`otlpjson.go`). A session's records leave in arrival order
  (`deliverMu`, held parts first). Every change to the relay's routing keeps
  `received = forwarded + dropped + queued_at_exit` (`TestRelayAccountsForEveryRecordUnderLoad`).
  A claim keeps up to 8 placements (`claim.Placement`): a session resumed in another bound
  repository gets a new one, and each record goes by the placement covering its sender, or,
  sender unknown, the one in effect at the record's time (`claim.At`) — the first run's late
  records stay with the first project.
- The sender of a connection is found with the kernel (`procinfo.FindSender`:
  `proc_info` on macOS, offsets pinned by a test against a real socket; `/proc` on
  Linux; `GetExtendedTcpTable` on Windows, untested on a real machine) once per connection at its first export, while the socket exists — never with
  lsof or another subprocess. A trace's session is learnt from spans *and* log records
  (Codex's mid-turn logs carry the trace id; its turn span arrives when the turn ends),
  and trace-keyed spans wait `TraceHold` (30 min); a full hold evicts the oldest, unnamed
  traces first. Claude's `UserPromptSubmit` hook (`user-prompt-submit`) exists to claim
  and start the relay at every turn's start — a relay that died between turns otherwise
  lost the next turn; it must print nothing (its stdout goes to the model).
- Routing is one decision (`relay.decide`, `internal/relay/route.go`): a part leaves when
  its session is claimed, covered and keyed, else it waits in the hold for that reason and
  is dropped under it. Nothing is placed on a guess: a span of a trace nothing has named
  waits for the trace (`TraceHold`); a part that names no session at all (Codex's
  metrics, its process-level spans) waits for its **sender to exit**
  (`Options.ProcessAlive`, `decideExited`, `exitGrace` 10 s for a shutdown flush) and
  then goes to that process's project only if the process named exactly one session in
  its life and that session is claimed — while it runs, a shared process (Codex's
  app-server) that has shown one claimed session may be about to name a personal one.
  The shared daemon rarely exits, so its unnamed work is dropped (loss, never a leak;
  Codex's usage also rides its logs). There is no adoption: an unclaimed conversation
  never leaves, whatever its policies or client (a developer's `codex -a never -s
  read-only` looks exactly like the TUI's title conversation). Inferred attribution is
  marked on the resource (`terma.relay.attribution=process`,
  `terma.relay.session.id`). A keyless claim waits too — the key may land mid-session.
- `claim.Write` is a read-merge-write under a sidecar lock (`<session>.json.lock`, 250 ms,
  falls through): unlocked, 14 of 16 concurrent writers' processes were lost.
- The relay runs as a per-user service by default: `terma install` sets it up
  (`ensureRelay`, `internal/cli/relay_service_choice.go`; `--relay-service off`, or `terma relay daemon
  remove`, opts out and is remembered in `relay/no-service`). launchd / systemd --user /
  on Windows the Run key starting `terma relay supervise`, named per config directory so
  sandboxes never collide: the only way to catch what an agent exports before its first
  hook. The manager restarts it only on a nonzero exit: `ExitRestart` (75) after stepping
  aside for a replaced binary, or a crash; exit 0 (its token gone) leaves it stopped. A
  service relay (`--idle 0`) that finds a hook-started one waits and takes over. Tests set
  `TERMA_RELAY_SERVICE=0` (and a test binary never registers one). A hook that had to start the relay waits up to 1 s
  for it to listen. `TERMA_RELAY_DEBUG=1` logs every drop.
- Heartbeat (`internal/relay/heartbeat.go`, `internal/relay/daemon/heartbeat.go`): every 15 minutes
  (`TERMA_RELAY_HEARTBEAT` for a test; the first a minute after start), for as long as the
  relay runs, one `terma.relay.heartbeat` OTLP log record — service.name `terma-relay`, **no
  project**: it is the organization's — posted as OTLP/JSON to the API gateway's
  `/v1/relay/heartbeat` with the developer's own credential (`api.SendHeartbeat`, a **stub**
  endpoint until the backend serves it; a failure is counted, `heartbeats_failed`, never
  queued — counters are cumulative). Facts only: terma version/os/arch/install kind, a
  random machine id (`relay/machine-id`), mode and content policy, the agents recorded and
  those whose exporters point at the relay, the agent builds seen in forwarded
  telemetry (`relay.agent.<service>.version`), when it last delivered, its counters
  (`relay.count.*`, unclassified keys only as a count), outbox and hold sizes. Never a
  hostname, a path under HOME or an email (`TestHeartbeatFactsNameNoOne`). Each beat says
  why (`terma.heartbeat.reason`: `start`, `interval`, or `setup`): `terma setup` ends by
  asking the running relay for one (`POST /heartbeat?reason=setup` on the relay,
  `daemon.CheckIn`) — the platform's "installed and working", and the developer's proof the
  relay, their credential and the endpoint work; the stub's 404 reads as "not taken yet".
- Compatibility matrix (`docs/COMPATIBILITY.md`, generated — never edit it by hand):
  every live scenario says which harness capability it proves (`Proves` / `ProvesAll`,
  `test/live/compat.go`; `Capabilities` is the row list, IDs append-only), and its outcome is
  written per run to `report/compat.json` (`report/linux/` from `make machines`). `make
  compat` (`test/live/compatgen`) merges runs into `docs/compat/history.json` (latest result,
  first pass, per build × platform × capability; a run that skipped a capability keeps
  what was known) plus hand-verified surfaces (`docs/compat/manual.json`: apps CI cannot
  drive), and renders the markdown and `docs/compat/compat.json` for the website. The
  nightly (`live.yml`) runs macOS and the Linux machine, renders the matrix, and pushes
  it to the `compat-matrix` branch, whose history each night extends. A new scenario
  that proves nothing in the matrix is a gap: tag it.
- `test/live/relay_workloads_test.go` runs each workload directly and through the relay and
  requires the same telemetry and zero drops; long live matrix runs use frozen copies of
  `bin/terma` and `bin/live.test`, or a rebuild mid-run mixes versions.
- omp goes through the relay by terma's Pi-family extension (`internal/agents/internal/pifamily/terma.ts`,
  agent "omp", `~/.omp/agent/extensions/terma-relay.ts`, lifecycle off — omp's committed
  hook file reports sessions and edits, the extension claims at each prompt with
  `omp-prompt`), never its native exporter: that reads OTEL_* only at startup, before any
  extension loads, so only a wrapper could set it, and omp's tools would inherit terma's
  token. omp's hook file and extension name the session
  `ctx.sessionManager.getSessionId()` — never an invented id.
- OpenCode goes through the relay too (`relay setup` points the plugin at it): its
  prompt rides a log body and its reply `gen_ai.completion`, both withheld with content.
  `test/live/opencode.go` fetches OpenCode builds from npm (`opencode-<os>-<arch>`), and
  `openAIChatProvider` is its fake model.
- Session keys: `session.id` (Claude, every signal), `conversation.id` (Codex logs), and
  `thread.id` on Codex's turn span only — a numeric `thread.id` is an OS thread and is
  never a session; sessionless spans go by their trace. Codex metrics carry no session and
  are dropped. Claude's tool content also rides a `tool.output` span event, which the
  golden attribute lists do not see.
- Desktop apps never run a PATH shim; the relay reaches them through user-level config.
  Claude Desktop runs its own pinned Claude Code (2.1.202) via the Agent SDK with
  `--setting-sources=user,project,local` under `service.name=claude-code-desktop` (the
  relay routes by session, never service name); its cowork VM loads no repository hooks
  and cannot reach the host's loopback. Codex Desktop, and since 0.157 a bare TUI when a
  daemon runs, run threads in `codex app-server`: one process for every workspace, which
  spawns the hooks and exports everything, so only `conversation.id` separates threads,
  originator is the *first* client's, and nothing it names no session for is attributed
  while it runs (see routing). `session_loop`'s `thread_id`
  (underscore) is a session key; an unclaimed `codex.conversation_starts` waits
  `TraceHold` (app-server exports it at `thread/start`, the first hook fires at the first
  turn). The daemon reads `[otel]` only at start: `relay setup` and doctor name
  `codex app-server daemon restart` (`runningDaemon` in internal/agents/codex), never run it.
  `test/live/codex_appserver.go` drives app-server over stdio JSON-RPC and a sandbox daemon
  (short `CODEX_HOME`: SUN_LEN); `test/live/claude_desktop.go` reproduces Desktop's launch.
- Pi, Hermes and DeepSeek Harness have no usable exporter: terma writes one into each —
  dsh's Cordis plugin (`$DSH_HOME/plugins/terma-relay.mjs`, inserted in
  `cordis.patch.yml`, auxiliary calls spanned through `llm/stream`), Pi's extension
  (`internal/agents/internal/pifamily/terma.ts`), Hermes's Python plugin (`internal/agents/hermes/plugin`,
  `$HERMES_HOME/plugins/terma`, enabled through `hermes plugins enable terma`; plugin
  hooks fire in every front end, shell hooks not in the TUI) — exporting GenAI spans
  (usage, cost) and prompt/reply logs under the agent's own session id, and calling
  `terma hook <agent>-*` by terma's absolute path (`termaHookCommand`: a desktop-started
  agent has the system PATH). `<agent>-prompt` claims without announcing. Their prompt
  and reply log bodies are in the relay's `promptBodyEvents`.
- Shims are being removed: nothing new may depend on one. omp's exporter reads OTEL_*
  only at startup, before any hook or extension loads (verified: a committed hook that
  sets them at load exports nothing), so omp has an exporter of its own like Pi's. Tools
  an agent runs must never inherit the relay's OTEL_* variables (its token): Claude Code
  strips them (`TestRelayClaudeToolsGetNoExporter`); omp gets none
  (`TestRelayOmpToolsGetNoExporter`). cursor-agent's own tracer is fixed to Cursor's backend, so Cursor never
  reaches the relay (`TestRelayCursorHooks`).
- Gemini CLI exports natively from `~/.gemini/settings.json` (`connectRelay` in internal/agents/gemini
  changes only its `telemetry` block); the file has no headers, so the relay also takes
  its token as the endpoint path's first segment (`relay.Handler`, exact, constant-time).
  Its claims come from terma's user-level Gemini extension (`~/.gemini/extensions/terma`,
  `terma hook gemini-*`). The content gate also strips resource attributes that restate a
  prompt (`resourcePromptFields`: Gemini's `process.command_args`), and the live leak
  check covers resources and metric points.
- With a project's content withheld the relay passes only attribute keys classified safe
  (`internal/relay/allow.go`, one set for every harness; a key that is content anywhere
  is content). Content keys keep content.go's marker/drop; any other key — on records,
  spans, span events, metric points, resources — is dropped and counted as
  `unclassified.<key>` (bounded), and a log body that does more than name its event is
  emptied. Live scenarios with content withheld fail on any unclassified key
  (`failUnclassified`): classify it, never widen the rule. `TestClassificationCoversTheGoldens`
  ties the list to the withheld-mode goldens.
- Never read harness log files to fill a gap: what the relay knows comes from OTLP and
  hook payloads. The pre-relay hook readers (Codex rollouts and `session_index.jsonl`,
  `~/.claude.json`; listed in `docs/RELAY.md`, "What still reads files on disk")
  stay only because what they capture — Codex's replies, thread titles, quota, Claude's
  account state — is exported nowhere else; nothing new may add one.
- OTLP types come from `go.opentelemetry.io/proto/otlp/{logs,metrics,trace}` as
  `*Data` messages (wire-identical to the export requests); never import the collector
  packages, which pull gRPC into every hook. Under the relay the machine-wide Codex config
  always allows prompts, so `repliesConsented` takes consent from the routing record
  alone. `test/live/relay_test.go` is the e2e proof and nightly canary (`golden/relay/`).

## Contracts other repos depend on

- Trailers: `Agent-Session-Id`, `Agent-Tool` (`internal/trailer`). The Terma backend
  and GitHub App parse these. Tool labels: `claude-code`, `codex`, `opencode`, `cursor`,
  `antigravity`, `omp`, `pi`, `hermes`, `gemini`, `dsh`.
- Hook event names are committed wiring and must stay stable: `session-start` /
  `session-end` / `post-tool-use` / `stop` / `stop-failure` / `subagent-start` /
  `subagent-stop` / `user-prompt-submit` (Claude Code's `.claude/settings.json`),
  `statusline` (Claude Code's user-level `statusLine.command`, written by `terma connect
  claude`), `codex-notify` (Codex's `notify`), `codex-session-start` /
  `codex-user-prompt-submit` / `codex-pre-tool-use` / `codex-permission-request` /
  `codex-session-end` / `codex-post-tool-use` / `codex-stop` / `codex-subagent-start` /
  `codex-subagent-stop`
  (Codex's `.codex/hooks.json`), `cursor-session-start` /
  `cursor-session-end` / `cursor-file-edit` / `cursor-post-tool-use` /
  `cursor-post-tool-use-failure` / `cursor-before-submit-prompt` /
  `cursor-after-agent-response` / `cursor-stop` / `cursor-pre-compact` /
  `cursor-subagent-stop` (Cursor's `.cursor/hooks.json`), `antigravity-pre-invocation` /
  `antigravity-post-tool-use` / `antigravity-post-invocation` / `antigravity-stop` (Antigravity's
  `.agents/hooks.json`),
  `opencode-session-start` / `opencode-session-end` / `opencode-file-edit` (called by
  the OpenCode plugin), `omp-session-start` / `omp-session-end` / `omp-file-edit` (omp's
  committed hook file and extension), `pi-session-start` / `pi-prompt` /
  `pi-session-end` / `pi-file-edit` (called by terma's Pi extension,
  `internal/agents/internal/pifamily/terma.ts`, which `terma relay setup --harness pi` writes),
  `gemini-session-start` / `gemini-prompt` / `gemini-after-tool` / `gemini-session-end`
  (terma's Gemini CLI extension), `dsh-session-start` / `dsh-prompt` / `dsh-session-end` /
  `dsh-file-edit` (terma's DeepSeek Harness plugin, `internal/agents/dsh/plugin/terma.mjs`),
  `hermes-session-start` / `hermes-prompt` / `hermes-session-end` / `hermes-file-edit`
  (called by terma's Hermes plugin, `internal/agents/hermes/plugin`). Both extensions share
  one handler set (`hookrun/extension.go`) and one payload shape. Cursor sessions are keyed on `conversation_id`, the one id
  present on every Cursor event; `afterFileEdit` has no `session_id`.
- Spool event names the platform parses (`gateways/otel/.../termacli_logs.go` and
  `termacli_entitlement_logs.go`): `terma.session.start`, `terma.files.touched`,
  `terma.session.observation`, the commit events, `terma.tool.call`,
  `terma.assistant.message` (Codex only — `text`, `message_id`, `trace_id`, `phase`;
  supplements Codex's native exporter), `terma.session.title` (Codex only — `title` from
  `session_index.jsonl`), and the funding
  events `terma.session.quota` / `.account` / `.limit` (folded by the entitlement adapter,
  live-ingesting in dev). Codex Desktop also emits `terma.turn.summary` (rollout turn
  status and available timing), `terma.compaction` (rollout compaction records), and
  `terma.approval.requested` (an approval request; the eventual decision is unavailable).
  `terma.tool.call` (Cursor `postToolUse` / `postToolUseFailure`,
  keyed on Cursor's `tool_use_id` as `tool_call_id`; `docs/CURSOR-INSTRUMENTATION.md`,
  "Tool calls" — and Antigravity's `PostToolUse`, keyed on `step-<stepIdx>`, unique only
  within its conversation; `docs/ANTIGRAVITY-INSTRUMENTATION.md`, "Tool calls and turns")
  carries a terma-made `turn_id`, as do Antigravity's observations.
  Every event from a linked git worktree carries `worktree` (git's name for it) and reports
  the main repository as `repo`; no adapter reads `worktree` yet.
  Still awaiting the adapter: `terma.subagent.start` / `terma.subagent.end` /
  `terma.subagent.call` and the `parent_session_id` / `agent_id` / `agent_type` /
  `agent_parent_id` attributes (`docs/SUBAGENT-INSTRUMENTATION.md`) are spooled today but
  no adapter folds them yet — they sit in the raw log store only. `terma.subagent.call` is
  deduplicated on `tool_call_id` and carries no token counts; `final_context_tokens` is a
  size, never a spend.
- The OpenCode plugin's OTLP shape — resource `service.name=opencode`, scope
  `terma-opencode`, spans `chat <model>` with `gen_ai.usage.*` and
  `gen_ai.usage.total_cost`, `execute_tool <tool>`, events named in `event.name` — is
  what the platform's `opencode` aisignal adapter parses.
- The relay heartbeat (`terma.relay.heartbeat`, OTLP/JSON logs, no project) posted to the
  API gateway's `/v1/relay/heartbeat` under the developer's CLI token: the endpoint is not
  built yet (`api.HeartbeatPath`). Collection policy is served by the account
  service (`api.CollectionPolicy`).
- Project header under a CLI token: `X-Mirador-Project` (`internal/account/api/client.go`).
  The shared gateway knows only that name; a Terma-branded header is a 400.
- `.terma/settings.json` (`internal/project`, JSON): `project{id,name,organization_id,
  environment}`, `install{hook_manager,hooks,terma_version,installed_at}`. No
  secrets, ever, and nothing per-developer: which agents a developer routes, and how, is
  home-directory state (`config.Profile.Harnesses`, the routing record), so a colleague
  re-running install never churns the committed file. Which agents' hooks are wired is
  not recorded either — the committed hooks files are the record (`Registry.WiredNames`:
  an agent is wired when its committed hooks file carries its entries). An `install.adapters` list
  once lived here and grew with each colleague's own agents; a binding that carries it
  loads and drops it on the next `Save`. Lives inside the same `.terma/` dir as the hook
  shims (`.terma/hooks/`).
- Browser login page: `<AppURL>/cli/auth?challenge&state&port&label[&org]` →
  `http://127.0.0.1:<port>/callback?code&state` (terma-frontend `src/features/cli`).
  `org` is an id or a name the page preselects; a hint only — the CLI compares the
  organization it gets back and says so when it differs.
- Release asset names: `terma_<Os>_<arch>.tar.gz|zip` with `x86_64` for amd64
  (`internal/selfupdate.AssetName`, `.goreleaser.yaml`, `install.sh`, `npm/install.js`).
- `install.sh` is itself a release asset (`release.extra_files`), and
  `https://terma.ai/install.sh` (terma-frontend `server/index.ts`) redirects to
  `releases/latest/download/install.sh`. `TERMA_RELEASE_BASE` in the script is for
  `scripts/test-install.sh` only; cleartext is accepted from loopback and nowhere else.
- The agent-facing guide at `https://terma.ai/cli/llms.txt` lives in terma-frontend
  (`public/cli/llms.txt`), not here. When a command, flag or output shape changes, that
  page needs the same change; the README and `docs/INSIGHTS.md` are what it summarises.
- Homebrew: the cask's quarantine strip is a `custom_block` carrying `preflight_steps`.
  GoReleaser's `hooks.pre.install` renders the `preflight do` block Homebrew deprecated
  (2026-08-04) and warns about on every install; `scripts/test-cask.sh` fails on it.

## Running it locally

There is no local account service, so a CLI login always uses the dev auth plane:
`TERMA_ENV=local` = local terma-frontend (`localhost:3000`, serves `/cli/auth`) in front
of `*-dev.mirador.org`; `TERMA_ENV=dev` = the deployed `dev.terma.ai` app in front of the
same backend. `terma config set --app-url/--auth-url/--api-url/--otlp-url` stores the same
thing on a profile. See docs/DEVELOPMENT.md, "Working against the dev backend".

`terma status` and `terma doctor` must agree about whether a setup works: both treat a
harness exporting to the right host but a *different project* as not connected
(`statusAgent` in `internal/cli/status.go` takes its verdict from `doctor.HarnessVerdict.Reaches`, which the `KeyHarness` check counts). Keep the
two in step — a status that says "connected" while doctor fails is worse than either.
The shared emission check fails zero-signal repository setups, reads Claude's merged
user/shared/private settings, and checks live routing's own signals. A route record
alone is not evidence of emission. Doctor verifies configuration and hook delivery;
it does not prove that a running agent has reloaded its settings or sent telemetry.

## Gotchas

- A command test that can reach `auth.Login` must pass `--no-browser` and run under a
  deadline (`within(d).combined`): the browser path opens the developer's real browser and
  waits five minutes.
- macOS: the first execution of a freshly written script pays ~200 ms in the OS exec
  policy check. It is one-time per file; `bench-hook` takes a median to ignore it.
- macOS has no `timeout`; tests use contexts, scripts use `perl -e 'alarm N; exec @ARGV'`.
- `git rev-parse --git-path hooks` honours `core.hooksPath` — a shim that used it
  chained itself. Chain `.git/hooks` (or `--git-common-dir`) and guard with `-ef "$0"`.
- Husky runs a hook file with `sh -e` and the file's exit status is its last line's. A
  guard shaped `command -v terma && { ... }` with nothing after it fails the commit
  (status 1) on every machine without terma. Every manager line ends in `|| true`;
  `TestManagerLinesAreInertWithoutTerma` runs each one terma-less, and `terma install`
  rewrites a stale husky/lefthook line in place.

- Updates (`internal/selfupdate`): normal successful interactive commands check daily;
  hook/shim/spool/version/update/completion, machine output, CI and
  `TERMA_NO_UPDATE_CHECK=1` skip passive work. `terma update --auto on|off|status`
  stores a machine-wide opt-in in `updates.json`. The release tag is the version:
  GoReleaser stamps it, and checks compare it with the latest published release. A
  source build (`make build`'s `git describe`, or `dev`) is never a release, so it is
  never nagged or replaced; `--force` explicitly switches one to a release. A
  checked-in version file once made every source build pass for the release it named,
  and a stale branch build became a replacement target. Downloads use checksum
  verification and atomic replacement under `update.lock`. An explicit `terma update` of a
  package-managed binary runs the manager that owns it (`selfupdate.ManagedBy`, read from
  the binary's path: that prefix's brew, or npm with `--prefix`); automatic updates only
  notify those. Failed checks retry after 15 minutes; auto-install attempts are throttled
  daily.
- Refresh (`internal/refresh`, `terma update --refresh`): after replacing itself or running
  the package manager, the old binary execs the new one's `update --refresh` — the old
  process cannot run new templates. It rewrites only files terma already wrote (the status-line wrap, the OpenCode plugin, and the current repository's hooks: the commit
  hooks through the binding's manager, the agent hooks its files already wire), never creates one, never signs in, and never changes a
  choice. Re-running `terma install` is not a substitute: it re-defaults every flag it
  does not record. The first interactive command under a newer release refreshes the
  home-directory files once (`refreshed.json`, upward only, so two builds on PATH do not
  take turns) and only *reports* stale committed files. Per repository, doctor and status
  name the same fix: committed hooks that exist but differ from this build's templates
  are out of date → `terma update --refresh` (`doctor.HookWiring.Stale`, and `doctor.AgentHooksCheck`
  for the agents' files); files that are missing → `terma install`. The binding's
  `terma_version` is the terma that last wrote the committed files: install stamps it only
  when it wrote one, refresh when it rewrote one (`Refresher.Stamp`, the checkout's own
  binding) — never a trigger, since contents decide staleness. `update --refresh` is a contract
  between releases: an older binary invokes it on a newer one, so it must keep working.
- Migrations (`internal/migrate`, registry in `migrations.go`): a change to the shape of
  state under the config directory ships with a migration, not a tolerant reader in the
  owning package (those were removed in 14a0037 and are not coming back). `cmd.Execute`
  runs pending ones before every command — hooks and shims too, silent and bounded to one
  second, because a hook is as likely as anything to be a new build's first run. The bound
  is a context the runner checks before each migration and every `Run(ctx)` checks between
  units of work; a run it cuts short records no failure and the next start carries on — and
  `update --refresh` retries a failed one and reports. Tests never pass through `Execute`,
  so none can migrate a real config directory. `migrations.json` records the last ID
  applied (one small read per start when nothing is pending). The rules: append-only IDs
  (never renumber, reuse or delete); idempotent; recognise the old shape exactly (a
  missing key, not a false one) because a fresh machine runs every migration against
  current state; leave state the previous build can still read — add and fill in, never
  remove or repurpose, since another terma may share the machine; home directory only
  (committed repository files are `--refresh`'s, on request). doctor has a `saved state
  migrated` line only when one is pending or failed. The registry is empty: ID 1 (it
  filled in `cli` on routing records 0.0.2 wrote) was retired with the field before
  release (`retiredThrough`), and its ID is never reused.

## Workspace installation regression tests

`make test-install-e2e` builds the actual CLI and exercises install/uninstall as
subprocesses with private configuration and no inherited credentials or exporters.
The matrix is documented in `docs/INSTALLATION-TESTS.md` and is also part of
`make check`. Installation uses `project.Locate`: Git determines a worktree root;
outside Git, the nearest binding or the first install's current directory does.
Non-Git session state lives under the config directory, keyed by canonical root.
Install reserves that directory before hooks can run, even with no session yet.
After `git init`, `project.StateDir` keeps selecting the existing private store,
so in-flight and newly started hooks use the same lock and manifests; do not
switch that workspace to an empty Git store on reinstall. Uninstall removes both
the private store/reservation and Git's hook-restoration journal. New Git-only
workspaces and linked worktrees continue to use their own Git metadata.
Git hooks use worktree-scoped config; never write shared `core.hooksPath` for a
linked worktree. Preserve user commands in mixed hook groups and refuse mutation
through symlinked configuration paths. An unknown or edited fallback shim must
not be overwritten or deleted just because it occupies `.terma/hooks/`.
