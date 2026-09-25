# terma-cli

Public Go CLI (`terma`) that connects coding agents to Terma and stamps the commits they
produce. Sibling of `../mirador-cli`, from which `internal/{auth,api,config,harness,output}`
were forked and rebranded (TERMA_* env, `~/.config/terma`). Keep those packages close to
their mirador counterparts; the product-specific code is everything else.

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
go test ./internal/hookrun/ -run TestName
```

Everything the Makefile runs uses `TERMA_ENV=dev` by default. Outside it, prefix the command
yourself (`TERMA_ENV=dev go test ./...`): `terma install` and `terma setup` sign in, so a bare
run that reaches them opens a browser login on **production**. A script that runs
`terma install` passes `--harness none`, which needs no credential.

## Shape

- Adapters (`internal/adapter`) are the one list of coding agents at repository scope:
  each says which file it writes (`hookmgr.Plan*`), which `terma hook <event>` names it
  handles (`hookrun.*`), which events flush, and — when the agent gates committed hooks
  behind a trust decision (Codex, Antigravity) — how to read that decision back.
  `install`, `uninstall`, `doctor` and `terma hook` iterate the registry; a new agent is
  one file there plus its hookmgr/hookrun halves, never another hand-written triplet in
  `cmd/`. The telemetry registry (`harness.All`) is a different, narrower list: only
  agents with a configurable OTLP exporter. Cursor and Antigravity are adapters but not
  harnesses.
- Hooks are thin shims; **all logic is in the binary** (`terma hook <event>`,
  `internal/hookrun`). Never put logic in `hookmgr.ShimScript` or the husky/lefthook/
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
  tail and never loop within a pass. Delivery failure → exponential backoff 30s..1h.
  Flushes are detached processes started by hooks: immediately after a commit or a
  session end and after end-of-turn capture (`stop`, `codex-stop`, `stop-failure`).
  Newly queued Claude status-line snapshots also trigger a flush because rendering
  can happen after Stop. Sender failure backoff still applies.
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
- Harness-agnostic: `internal/harness` is the only place that knows a harness's
  file layout. `hookrun` speaks in events (session start/end, files touched) and
  the tool label travels as data. The `Harness` interface is what a command needs and no
  more: how a harness translates an exporter (`render`) is unexported, because the three
  translate into three different things. What only some harnesses can do is an optional
  interface in `internal/harness/optional.go` (`Noter`, `Credentialed`, `Backuper`, and
  `Scoped`), each with a `var _` assertion — a capability asked for by type assertion
  does not fail to compile when its method drifts, it silently switches off.
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
- `internal/hookmgr` by file: `hookmgr.go` is the vocabulary (`Manager`, `Detect`,
  `Change`, `Plan`, `Apply`); `git.go` the four git hook managers; `claude.go`,
  `codex.go`, `cursor.go`, `antigravity.go` one agent each; `json.go` the unescaped,
  key-ordered JSON. The three event-keyed hooks files (Claude, Codex, Cursor) share
  `mergeEventHooks` (`events.go`): a planner only builds its entries, and
  `hooksFile.Defaults` carries Cursor's `version` rule. Antigravity's file is keyed by
  hook name and plans itself. `readFile` returns an error for anything but "not there".
- `internal/hookrun` by file: `events.go` holds every spool event name and the repeated
  attribute keys and values (`TestWireNamesAreFrozen` pins the strings — add a constant,
  never rename one); `input.go` is the one bounded payload reader (`readHookInput[T]`,
  4 MiB, an oversized payload is refused by name); `lifecycle.go` the session
  start / end / files-touched steps every agent shares (`announce`, `endSession`,
  `touch`); `attrs.go` the evidence header and the bounded-string guard; `state.go` the
  state directory names and timing constants. A handler resolves the repository once
  and passes the `*repo` down. Only Codex patches and Cursor's `subagentStop` dedupe and
  sort their `files` list (`uniqueSorted`, at the call site); every other agent keeps
  the reported order.
- doctor: `terma on PATH` warns when a *different build* of terma (by content hash — it
  never runs what it finds) sits elsewhere on PATH or in `wellKnownBinDirs`: an app started
  from the Dock gets the system PATH, so `/usr/local/bin/terma` is what Cursor's hooks run,
  and a build from before `.terma/settings.json` does not see the binding. Tests blank
  `wellKnownBinDirs`. The hooks check is two: `commit hooks installed` (git wiring, all or
  nothing) and `agent hooks run` (`agentHooksCheck`, shared with status — a fraction,
  `Check.Ready`/`Of`). A commit is stamped with the session that touched its files and a
  session exists only because its agent's hooks announced it. The readiness checklist
  names agents awaiting trust instead of assigning speculative spend percentages.
  Doctor also compares PATH's executable with the running build; a mismatch can leave
  scratch events without a project binding. Only the developer's own agents
  (`config.Harnesses`; empty = all) count — a colleague's committed Codex hooks are not
  theirs to trust.
- The command surface is small on purpose (`cmd/command_surface_test.go`). `terma --help`
  lists `primaryCommands` — setup, install, status, doctor, session, usage, blame, org,
  uninstall, update — and everything else is `Hidden: true`, **not removed**: login/logout/
  whoami, connect/disconnect/telemetry/harness, project, principal, config, spool, version,
  hook, shim. Hidden commands are what automation and CI run and what terma's own fix-it
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
- `terma setup` signs in and records the developer's agents (`config.Profile.Harnesses`) and
  does nothing else — no project, no key, no connection, no file. Everything per-repository is
  `terma install`, including what `setup` used to do on the way: per-clone `core.hooksPath`
  wiring, the Claude status-line wrap, and the **spool key**. That key (`keystore.Get(project)`)
  is what hook events are delivered with; pointing a telemetry agent stores it as a side effect
  (`keystore.SetFor`) and nothing else did, so a developer whose agents are all hooks-only
  (Cursor, Antigravity) had every event held forever. `ensureSpoolKey` mints one when hooks are
  wired and none is stored; install signs in for it only when the developer selected an agent —
  `--harness none` (CI, onboarding a repository) stays credential-free and is told its events
  are held. Fix-it hints follow the same split: sign-in → `terma setup`; a missing key, an
  unwired clone, an agent on the wrong project → `terma install`. With a server key
  (`TERMA_API_KEY`) install signs in to nothing and binds the key's own project, read from
  the API gateway's `/v1/identity` (`serverKeyBinding`): the account service's
  `/v1/projects` accepts only a signed-in user, and a `--project` or existing binding
  naming another project is refused, since the key could not deliver its events.
- Per-repo routing (`internal/shim`, `terma shim prepare <agent>`): `terma install` points
  each agent at the repository's project; secrets stay in the home directory,
  namespaced by project id (`routing/<id>.json`, `claude/<id>/`, keys in the keystore).
  PATH scripts and `--activation wrapper` functions share the shell launcher. It
  resolves the real agent from PATH, bounds Terma preparation to two seconds, reads
  a versioned argument-file protocol without eval, and execs the agent exactly once.
  Preparation failure passes through; `TERMA_DISABLE=1` and leading maintenance
  commands bypass preparation entirely. Never retry after the agent has started.
  Claude uses `--settings <claude/<id>/settings.json>` with a dedicated headers helper.
  Native tests on 2.1.270–2.1.272 show this overrides global/shell detailed beta
  tracing where repository-local settings cannot. Generated settings mask per-signal
  overrides and competing beta destinations. An explicit user `--settings`, or a
  failed settings write, passes through without an environment fallback.
  Codex keeps the original `CODEX_HOME` and
  receives per-launch telemetry through `-c` overrides, including the bearer key.
  The key is visible in process arguments; never log the generated argv. Config,
  keyring login, trust, history, and notify remain in the original home. Routing
  resolves `-C` / `--cd` before choosing the project. Funding comes from repository
  `.codex` hooks. OpenCode routes itself: the plugin's `perRepo`
  mode reads the binding and picks `helpers/opencode-otel-<id>`; the id is validated in
  the plugin exactly as `project.ValidID` does, because the binding is a committed file
  and the id names a script the plugin executes. "Live" (`shim.Active`) means the agent's
  name resolves to the shim — on PATH *ahead of* the real binary — or the wrapper is
  loaded; `status` and `doctor` report shell activation independently of export,
  including missing opt-in and routes not configured for this project. Inactive
  routing warns when global export still works; an export with no working route
  fails. PATH entries are compared by identity (`sameDir`), not spelling: a trailing
  slash that slipped past `RealBinary` would make the shim exec itself forever.
  The shim directory gets onto PATH through the developer's shell startup file
  (`internal/shim/rc.go`): install asks (`--yes` consents, `--no-path` declines and prints
  the line), then writes one marked block **at the end** of `~/.zshrc` / `~/.bashrc` (macOS:
  an existing `~/.bash_profile`) / fish `conf.d/terma.fish`. Last, because a PATH line only
  beats the ones after it and a real startup file prepends `~/.local/bin` — where the real
  binaries live — several times; a pasted line was silently overtaken. `RC.State`
  tells absent / last / overtaken (a later line sets PATH — `pathEdit`, which must not
  match GOPATH or MANPATH), `Ensure` appends or moves the block and keeps every other
  byte (writing *through* a symlinked dotfile), `Remove` restores the file exactly, and
  `shim uninstall` calls it. doctor's fix is specific: "open a new terminal" when the block
  is last and this shell predates it, else `terma install`. Never write a startup file
  without consent, and any test that can reach `RemoveAll` or `putShimsOnPath` must
  sandbox `HOME` and set `SHELL`.
- Codex is split across two scopes and neither is optional. Telemetry supports user-level and runtime configuration: Codex strips `otel` (with `notify`, `profile`, `profiles` and the provider keys)
  out of a project's `.codex/config.toml` and warns at startup, so `--scope local` has
  nothing to write. `terma connect codex` writes user-level `config.toml`;
  `terma install` configures the shim to pass runtime `-c` overrides. The repository half is hooks
  (`internal/hookmgr/codex.go`): `SessionStart`, `PostToolUse`, `Stop` and `SessionEnd` in
  `.codex/hooks.json`, which Codex loads only for a trusted project *and* only after the
  developer trusts each entry from inside Codex. Until then the file is inert and
  nothing says so, which is why `doctor` reads the `[hooks.state]` record in the user's
  config (`harness.CodexHookTrustFor`) and reports untrusted hooks as a warning. Terma
  never writes that table.
- Codex never exports what it **said**: its OTel events carry `prompt` and tool
  `arguments`/`output`, `response.completed` is token counts, and no switch adds the reply
  (every record of a live 0.155.1 session, 2026-09-19). So `codex-stop` (and `codex-notify`,
  `codex-session-end`; one locked cursor per session under `reply-cursors/`) reads the
  turn's assistant messages from the rollout (`harness.ReadCodexReplies`, the same confined
  open as funding, 32 messages / 1 MiB per invocation, 16 KiB per message) and spools
  `terma.assistant.message`. It is the **one** place terma reads conversation content, and
  it is gated on the consent prompts travel under (`codexRepliesConsented`): the repo's
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
- Codex names no edited file: `PostToolUse` carries the tool call, and the paths live in
  the apply_patch envelope inside `tool_input.command` (`hookrun.applyPatchPaths`). That
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
- `terma install` writes the repository half of that arrangement by default into the
  same committed `.claude/settings.json` the hooks live in, after the hook plan applies
  so both merges land in order. Re-running install preserves an existing policy unless
  export flags explicitly change it. Per-repo routing is configured independently.
  A pre-existing OTLP conflict there is reported and
  skipped, not fatal — the hooks and the binding are already written by that point.
- Connect scope (`internal/harness/scope.go`): `Claude{}` is global; `Claude{}.Local(root)`
  writes `<root>/.claude/settings.json` and renders only `claudeLocalKeys` — the three
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
- Sessions (`internal/auth/store.go`): `credentials.json` holds, per profile, one
  credential per organization plus which is active (`{active, organizations}`).
  `cmd.signIn` is the only way a command obtains a credential: verify the stored one
  (`/v1/whoami`, which also refreshes it), reuse it, and open the browser only when
  the organization has none. `SaveCredential` activates and reports the session it
  displaced (revoked best-effort); `UpdateCredential` is the refresh path and never
  changes which organization is active. `logout` revokes every stored session.
- `terma org use` (`cmd/org.go`) changes account scope only. `terma install` selects
  and saves projects per repository, and reinstalls reuse that binding. Project-scoped
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
- Terminal courtesies (`internal/style`, `internal/spinner`): colour and the
  four-square spinner draw only on a terminal a person is watching — never in a
  buffer, a pipe, an agent (`CLAUDECODE` and friends), `NO_COLOR` or `TERM=dumb` —
  so tests compare plain strings. `doctor` streams each check as it finishes
  (`doctorProgress`) and polls the round-trip every second.
- Interactive prompts (`internal/prompt`): a pure form model plus a raw-mode driver on
  `x/term`, no TUI dependency. Shown only when `canPrompt()` (stdin/stdout/stderr are
  terminals, no agent env var); every box has a flag and `--yes` skips the form.
- OpenCode (`internal/harness/opencode.go`; plugin `internal/harness/opencode/terma.js`,
  embedded): the harness is a dependency-free plugin written whole into
  `~/.config/opencode/plugins/terma.js` with one `const CONFIG = {...}` line spliced in
  (the raw template ships `CONFIG = null` and is inert). The user's `opencode.json` is
  never touched; the key stays in the headers-helper script. `bun test
  internal/harness/opencode` drives the plugin the way OpenCode would (`make check` runs
  it when bun is present). Local scope is `<root>/.opencode/terma.json`, policy only.
- The `project` package is imported as `termaproject` in `cmd/` because `cmd` has a
  ported `type project struct`.
- Read commands (`usage`, `session`, `principal`; `cmd/usage.go`, `cmd/session.go`,
  `cmd/principal.go`) are thin over the API gateway's `/v1/ai/*` and
  `/v1/metrics/query` (`internal/api/ai*.go`, `metrics.go`). `usage` is PromQL
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
- `blame` (`cmd/blame.go`) is the reverse join: a commit → the session that produced it.
  It reads the commit locally for its sha and time, then reads back the `terma.commit`
  record the post-commit hook exported, via `CommitLog` over the log store's `/v1/logs`
  query surface (`internal/api/logs.go`) — filter `attribute.event.name="terma.commit"
  AND attribute.sha=…`, windowed ±1h on the commit's own time because the store caps a
  query's span (currently 840h). Records come under `logs`, every attribute value is a
  string nested under `attributes`/`resource_attributes` (hence `LogRecord.Attr/Int`).
  No cost yet: the trailer/session.id is not what the usage metrics key on.

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

- Status line (`internal/hookrun/statusline.go`, `internal/harness/claude_statusline.go`):
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
- Antigravity CLI (`agy`, Google's Gemini CLI successor; `internal/hookmgr/antigravity.go`,
  `internal/hookrun/antigravity.go`, `internal/harness/antigravity.go`): hooks only, no
  OTLP export. `.agents/hooks.json` is keyed by hook *name*; terma owns the `terma` key
  whole (other names untouched, a developer's `"enabled": false` preserved) with
  `PreInvocation` / `PostToolUse` (unmatched) / `PostInvocation` / `Stop`, never
  `PreToolUse` (it demands a decision). Payloads are protojson camelCase; the session key
  is `conversationId` (also `ANTIGRAVITY_CONVERSATION_ID` in the hook's env); the
  repository is `workspacePaths[0]`, and the hook's cwd is `<repo>/.agents`. Edit paths
  come from `toolCall.args.TargetFile` (agy's own tools) or the vendor-style keys. Every
  handler prints `{}` to stdout — agy's documented reply — so `hookrun.Env.Stdout` is now
  set for all hooks. agy loads hooks only in a **trusted** workspace (`trustedWorkspaces`
  in `~/.gemini/antigravity-cli/settings.json`, read by `harness.Antigravity`) and only
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
  invocation 0>`, recorded by `PreInvocation` (`hookrun/antigravity_turn.go`,
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
  Tool calls (`internal/hookrun/cursor_tool.go`): `postToolUse` / `postToolUseFailure`
  spool `terma.tool.call` — `tool_name`, `tool_call_id` (Cursor's `tool_use_id`),
  `duration_ms`, `status`, `failure_type`, `is_interrupt` — bypassing the observation
  checkpoint because the native id is the replay key. `tool_input`, `tool_output`,
  `error_message` and `cwd` are never read; a call attributes no file and starts no
  flush. The generic pair is the only one wired: Cursor's `before*` gates sit on every
  call's critical path and fail open only by default (a schema-mismatched reply or
  `failClosed` blocks), and `afterShellExecution` / `afterMCPExecution` restate the
  same calls without a `tool_use_id` and with their output.
- Subagents (`internal/hookrun/subagent.go`, `docs/SUBAGENT-INSTRUMENTATION.md`) have two
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
  reads that rollout's first line for the spawn record (`harness.CodexRolloutSpawn` →
  `agent_parent_id`, `agent_depth`, `agent_nickname`, `agent_path`, `rollout_status`);
  `SubagentStop` is per child *turn*, not a bracket. Capture inside the thread must read
  the child's rollout — `codexRolloutID` takes the thread from the rollout file's own
  name — or the confined open refuses it as another thread's and every quota and reply
  there is lost. Codex's source fires no `SessionStart` for a spawned thread; the one-line
  read there stays in case a build does.
  **Cursor** and **OpenCode** give the child a conversation or session of its own.
  OpenCode's `terma.session.start` carries `parent_session_id`, its spans
  `opencode.parent_session.id`, and the child is never made active — it would claim the
  developer's next hand-written commit. Cursor's `subagentStop` is filed under
  `parent_conversation_id`, names the subagent as `agent_id`, and folds a manifest the
  subagent built under its own conversation id into the parent's (`session.Store.Merge`),
  so the commit is stamped once. Cursor's `subagentStart` is never wired — a hook printing
  nothing blocks the spawn, and the committed guard prints nothing without terma.
  **Antigravity** names no subagent in its payloads.
  No transcript, task text, description or last message is read.
  Codex trusts hooks entry by entry: a developer who trusted terma's before the two
  subagent entries existed has a file that counts as trusted and two hooks Codex skips in
  silence, so the adapter compares entries (`hookmgr.CodexTermaEntries`,
  `CodexHookTrust.TrustedKeys`) and doctor names what is skipped.
- `docs/collection-matrix.html` is the harness × information × mechanism matrix (open it
  in a browser). Update a cell when a mechanism ships or a live check changes it.
- `live/` runs the real harness binaries with real credentials through the real `terma`
  binary and checks the matrix's promises across the hook spool, an in-test OTLP receiver
  and a pseudo-terminal (`make live` there; own Go module, built in Docker, run natively).
  Interactive sessions go through a pty because the status line and the trust dialog only
  exist there; `golden/` holds each surface's attribute names so drift fails a test. A
  scenario without its credential is reported as not run, never as a pass.

`pocs/funding-model` is the estimator for *who paid* for a model call (seat allowance,
usage credits, API metering) and an evaluation harness that runs it over simulated
organisations with known truth and the providers' exports rendered from that truth. It
is a separate Go module with no dependencies and runs only through Docker (`make eval`
there); `model/` is written to move to the backend unchanged. The estimator never
determines a route: it scores evidence (account snapshot from `~/.claude.json`,
credential hints, per-request `speed`, Codex `auth_mode`) and is corrected by
reconciliation against simulated user × model × day USD reports. The local
`cmd/funding-reconcile` pilot preserves the actual exports' grain and units (Claude
account/model/period USD; OpenAI workspace/product/interval credits); see
`pocs/funding-model/replay/README.md`. Do not feed real exports to the simulation
reconciler or price real calls with its illustrative tables. The earlier Codex
exploration it superseded (`pocs/funding-observer` and two handover files in the root) was
removed on 2026-09-21; it is in the history before that. The one piece of it still cited, the
provider report schema evidence, lives in `pocs/funding-model/replay/evidence/`.

## Contracts other repos depend on

- Trailers: `Agent-Session-Id`, `Agent-Tool` (`internal/trailer`). The Terma backend
  and GitHub App parse these. Tool labels: `claude-code`, `codex`, `opencode`, `cursor`,
  `antigravity`.
- Hook event names are committed wiring and must stay stable: `session-start` /
  `session-end` / `post-tool-use` / `stop` / `stop-failure` / `subagent-start` /
  `subagent-stop` (Claude Code's `.claude/settings.json`),
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
  the OpenCode plugin). Cursor sessions are keyed on `conversation_id`, the one id
  present on every Cursor event; `afterFileEdit` has no `session_id`.
- Spool event names the platform parses (`gateways/otel/.../termacli_logs.go` and
  `termacli_entitlement_logs.go`): `terma.session.start`, `terma.files.touched`,
  `terma.session.observation`, the commit events, `terma.tool.call`,
  `terma.assistant.message` (Codex only — `text`, `message_id`, `trace_id`, `phase`;
  supplements Codex's native exporter), and the funding
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
- Project header under a CLI token: `X-Mirador-Project` (`internal/api/client.go`).
  The shared gateway knows only that name; a Terma-branded header is a 400.
- `.terma/settings.json` (`internal/project`, JSON): `project{id,name,organization_id,
  environment}`, `install{hook_manager,hooks,terma_version,installed_at}`. No
  secrets, ever, and nothing per-developer: which agents a developer routes, and how, is
  home-directory state (`config.Profile.Harnesses`, the routing record), so a colleague
  re-running install never churns the committed file. Which agents' hooks are wired is
  not recorded either — the committed hooks files are the record (`adapter.WiredNames`:
  an adapter is wired when its uninstall plan is non-empty). An `install.adapters` list
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
(`harnessState` in `cmd/status.go`, the `KeyHarness` check in `cmd/doctor.go`). Keep the
two in step — a status that says "connected" while doctor fails is worse than either.
The shared emission check fails zero-signal repository setups, reads Claude's merged
user/shared/private settings, and checks live routing's own signals. A route record
alone is not evidence of emission. Doctor verifies configuration and hook delivery;
it does not prove that a running agent has reloaded its settings or sent telemetry.

## Gotchas

- A command test that can reach `auth.Login` must pass `--no-browser` and run under a
  deadline (`runTermaWithin`): the browser path opens the developer's real browser and
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
- Refresh (`cmd/refresh.go`, `terma update --refresh`): after replacing itself or running
  the package manager, the old binary execs the new one's `update --refresh` — the old
  process cannot run new templates. It rewrites only files terma already wrote (shims,
  the status-line wrap, the OpenCode plugin, and the current repository's hooks: the commit
  hooks through the binding's manager, the agent hooks its files already wire), never creates one, never signs in, and never changes a
  choice. Re-running `terma install` is not a substitute: it re-defaults every flag it
  does not record. The first interactive command under a newer release refreshes the
  home-directory files once (`refreshed.json`, upward only, so two builds on PATH do not
  take turns) and only *reports* stale committed files. `update --refresh` is a contract
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
  migrated` line only when one is pending or failed. The first, ID 1, fills in `cli` on
  routing records 0.0.2 wrote: without it the router read the missing field as false and
  stopped routing the Codex CLI.

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
