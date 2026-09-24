# Design notes

The decisions that shaped `terma`, and why. Read this before changing the hook path.

## Hooks are shims; terma is the runtime

Every hook `terma install` writes — the git hooks, Claude Code's `SessionStart` /
`PostToolUse` / `SessionEnd`, Codex's `notify` — is a one-liner that calls
`terma hook <event>` and exits 0 no matter what. Session files, touched-file manifests,
trailer stamping, and event spooling all live in the binary.

Why: the committed files are the part that is expensive to change (a PR per repository)
and the binary is the part that is cheap to change (`terma update`). Putting logic in the
files would freeze the first version of that logic into every repository that installed
it.

The one thing every committed line does besides calling terma is guard the call:
`command -v terma` first, `|| true` last. The files land in a repository through a PR,
so they run on every colleague's machine, including those who never installed terma,
and for them the hooks must be invisible: exit 0, no output, the message untouched
(`TestManagerLinesAreInertWithoutTerma` runs each manager's line that way, and
`TestHarnessHookCommandsAreInertWithoutTerma` each harness entry). The harness entries
need the guard as much as the git hooks: Claude Code prints a hook's stderr in the
transcript when it exits non-zero and hands SessionStart and PostToolUse stderr to the
model as context, so an unguarded `terma hook` was "command not found" after every
edit, read by the model. One string, `hookmgr.HookCommand`, is every harness entry;
changing it rewrites every committed hooks file on its next install and, for Codex,
which trusts a hook by the hash of its entry, asks each developer to trust once more. The
trailing `|| true` is not decoration. Husky runs a hook file with `sh -e` and takes the
file's exit status from its last line; an earlier husky line guarded the call with
`command -v terma && { ... }` and nothing after it, so on a machine without terma the
guard's own status became the hook's and husky failed the commit. `terma install`
rewrites a stale line in place, so a repository picks the fix up on its next run. For a
developer who has terma and wants the hooks off, `TERMA_HOOKS=0` makes `terma hook` an
immediate exit 0 — the same convention as `HUSKY=0` and `LEFTHOOK=0`.

## The two budgets

1. `prepare-commit-msg` completes in under 50 ms with zero network.
2. No hook ever blocks on Terma being reachable.

A hook that makes commits slow, or fails one because a backend was down, gets
uninstalled — and takes attribution with it. So:

- Hooks only append to a local spool (`~/.config/terma/spool/events.jsonl`). A detached
  `terma spool flush` delivers after `post-commit` and session end, with exponential
  backoff (30 s → 1 h) on failure and a 16 MB bound. Whatever the bound costs is
  reported by reason — expired past 14 days, pruned for disk, an unreadable line — so
  bounded loss is never mistaken for a pruned queue or a torn write. The same fast path
  is why a failed append is silent, and why `doctor` probes the spool with a write
  rather than trusting a read: unreadable-to-write and empty look identical from here.
- The hot path avoids git subprocesses (13 ms each on macOS): the repository is located
  by walking to `.git` (following `gitdir:` files for worktrees), `core.commentChar` is
  read from the config files directly, and `post-commit` reads sha, author, message, and
  per-file line stats in a single `git log --numstat`. The one unavoidable call is
  `git diff --cached` — and it is skipped entirely when no session manifest exists (a
  human-only commit).
- Measured on an M-series Mac: shim end to end ≈ 25 ms without a manifest, ≈ 33 ms with
  one. `make bench-hook` fails CI above 50 ms.

The first execution of a freshly written script on macOS costs ~200 ms in the OS exec
policy check. That is one-time per file (the commit right after `terma install`), not a
per-commit cost; the benchmark takes a median to see past it.

## Attribution rules

A manifest is a per-session record of files the agent edited (from the harness's own
edit events). `prepare-commit-msg` intersects the staged files with every manifest and
appends one `Agent-Session-Id` / `Agent-Tool` pair per session with a non-empty
intersection. Multiple sessions → multiple pairs; human-only work → nothing.

Fallback: a harness that cannot report files gets the *active-session-with-TTL* rule — a
session announced within four hours claims the commit — but **only when that session has
no manifest at all**. If a session reported files and none are staged, the commit is not
its work. `Consume` therefore keeps an empty manifest after a commit: the emptiness is the
evidence.

Merge and squash messages are never stamped; `post-commit` retires the committed files
from their manifests so the next commit is not attributed to work that already shipped.

The `terma.commit` event carries the commit's per-file line stats (`file_stats`, a JSON
array of `{path, added, deleted, binary, session_id}`, capped at 50 entries with
`file_stats_truncated` when it is cut, plus the `lines_added` / `lines_deleted` totals).
Those counts are the **commit's** delta, not a measurement of what the agent wrote: a
human editing the same file in the same commit is inside the number, and a manifest
records that a session touched a file, not which lines it produced. It is an upper bound
on agent-written change and any UI must label it as one.

### Every commit is counted

The coverage meter's denominator is "commits made", so `post-commit` emits an event for
a commit that carries no trailer too: `terma.commit.unattributed`. It is a separate
event name rather than an `attributed=false` flag on `terma.commit`, so nothing that
reads `terma.commit` — and relies on `sessions` always being present — has to change,
and neither event needs a discriminator attribute. Coverage is still one log filter:
`event.name IN ('terma.commit', 'terma.commit.unattributed')`, grouped by name. Not a
prefix match: `terma.commit.stamped` is the prepare-commit-msg record of the same commit
and would count it twice.

The unattributed event is deliberately minimal. Terma had no part in the commit, so it
reports the commit's identity and size — `sha`, `repo_url`, `branch`, `terma.repo`,
`author_email`, `file_count`, `lines_added`, `lines_deleted` — and nothing about its
contents: **no file paths, no `file_stats`**. The denominator is a count, never a
manifest of what a human changed; anything added to that event has to pass the same
test. It costs no extra work: everything on it was already in hand from the one
`git log` post-commit runs, and the remote is read from the config file the way
`core.commentChar` is (`gitx.RemoteURLFS`), which also took the one remaining git call
out of the stamped path.

Merge and squash commits are skipped as `prepare-commit-msg` skips them, or they would
sit in a denominator they can never join. `post-commit` gets no source argument, so a
merge is read off the parent count that comes with the same log call — `git merge`
itself never runs post-commit (it runs post-merge); only a conflicted merge finished
with `git commit` reaches it — and a squash off git's default "Squashed commit of the
following:" subject. `SQUASH_MSG` is already unlinked by then, so a squash whose message
was rewritten by hand is counted as the ordinary single-parent commit it is.

## Repo scope vs. user scope

`terma install` writes only things that belong in the repository and carry no secrets:
hook wiring through the manager the repo already uses (husky, lefthook, pre-commit) or
committed shims plus `core.hooksPath`; `.claude/settings.json` project hooks; and
`.terma/settings.json` binding the repo to a project. One merged PR onboards everyone.

`terma install` also does the per-person, per-repo half in the same run: sign in (if
`terma setup` has not), identity (`git user.email` as `enduser.id` on Codex and OpenCode
sessions), and pointing each agent at *this repository's* project — Claude Code through
a per-repo settings document passed as `claude --settings` (the command line outranks
`~/.claude/settings.json`; the process environment does not, so exporting `OTEL_*`
around the agent loses to a machine-wide connect), Codex through per-repo
`-c` telemetry overrides while preserving its original home, OpenCode through its project-aware plugin. Keys live in the home directory, namespaced by project. `terma
setup` is the optional machine-level preamble: sign in and record which agents you use.
Events spooled before a key is stored are **held**, not dropped, so ordering loses no
data.

### Decision: retain the Claude launcher and bound preparation

Claude still receives a private settings document through `--settings`. Tests against
Claude Code 2.1.270, 2.1.271, and 2.1.272 with local mock API and OTLP receivers found
that explicit settings override global/shell detailed beta tracing; repository-local
`settings.local.json` does not reliably override that exporter. This is a precedence
finding, not evidence that detailed beta tracing is deprecated. The route explicitly
sets its per-signal endpoints, protocols, and headers, and disables the competing
beta destination for that launch. Global and repository settings are left intact.
Generated settings and the credential helper are refreshed on every launch.

Keeping launch-time resolution also lets separate worktrees select separate projects
without writing shared Claude local settings. The cost is intercepting the agent's
launch and depending on its CLI/configuration contract. An explicit user `--settings`
causes Claude routing to pass through unchanged; a failed settings write does the same.
We removed the environment fallback because it cannot reliably override global
settings and can produce a partially configured export.

The existing shell script, not another binary, now owns launch:

- Resolve the real agent from the current PATH on every invocation, skipping the shim
  and aliases by filesystem identity. Homebrew/other replacement paths are not cached.
- `TERMA_DISABLE=1` (or `true`) bypasses Terma completely. Leading help/version,
  update/install, auth/login/logout, and completion commands also bypass preparation.
- Run `terma shim prepare` with a two-second deadline and disconnected stdin.
  Preparation writes a versioned data plan in a private temporary directory: one file
  per prefix argument and a completion count. The shell never sources or evals it.
  Codex credentials temporarily occur in these mode-0600 argument files, inside the
  mode-0700 directory; normal completion and handled cancellation remove them.
- Missing/crashed/hung/incompatible Terma or an invalid plan launches the original
  agent arguments. Preparation failures emit a warning; missing Terma is transparent.
- After successful preparation, `exec` the agent once. Stdio, PID, signals, and exit
  status belong to the agent. Never retry after an agent has started: a nonzero exit
  can follow real edits or billable work. Cancellation during preparation does not
  launch the agent. Shell functions use the same installed launcher.

This does not guarantee compatibility with every future agent version. A CLI that
rejects an injected option after launch returns its own error; use `TERMA_DISABLE=1`
to bypass it. Administrator policies and explicit user overrides remain authoritative.
An uncatchable kill can leave temporary files until OS/user cleanup. Preparation
errors may mean the user's pre-existing global exporter applies instead of Terma.

Regression coverage includes launcher fault injection, argument quoting/newlines,
PATH changes and aliases, stdin/stderr/exit preservation, cancellation, and PID/signal
behavior. The PTY tests also check all three terminal descriptors, interactive
input, resize/SIGWINCH delivery, terminal-generated Ctrl-C during preparation and
agent execution, exit status 130, temporary-file cleanup, and returning to a usable
shell. They exercise both PATH and function activation under available sh/bash/zsh/dash
shells. CI runs launcher contracts on Linux and macOS. The PTY driver uses Python 3's
standard library; no extra launcher executable or production dependency is introduced.
`TestClaudeRouteNativeExport` is an opt-in local contract test, enabled with
`TERMA_CLAUDE_NATIVE_TEST=1` and optional `TERMA_CLAUDE_BINARY`; it uses no real provider
credentials or inference. A future stage should wire these native contract checks
into the periodic version matrix and assess safe pre-launch compatibility probes.

### Decision: Codex telemetry through runtime overrides

The routing shim keeps the developer's original `CODEX_HOME` (including a custom
one) and passes this project's telemetry settings through Codex's `-c` runtime
overrides. It resolves `-C` / `--cd` before selecting the repository binding and
reads the project key from the keystore on each launch. Outside a configured
repository it passes through without adding telemetry settings. Existing global
telemetry configuration still applies there; this is not a global telemetry opt-out.

We chose this over maintaining a per-project copy of the Codex home:

- The real configuration remains authoritative, so changes to models, providers,
  MCP servers, and other settings take effect without reinstalling Terma.
- Authentication stays in its original location. In particular, Codex's keyring
  lookup is tied to its home directory; symlinking `auth.json` alone does not
  preserve keyring-backed login when the home changes.
- History, skills, project/hook trust, and the developer's notifier remain in place.
  We avoid copying configuration, merging trust back, or synchronizing symlinks.
- Key changes take effect on the next launch. No generated Codex config contains
  a second copy of the credential.

The tradeoff is credential visibility: the exporter Authorization header is passed
as a literal in `-c` arguments. It can therefore appear in process inspection and
tools that record command lines. Terma must not log the generated argv. This path
does not inject the credential into environment variables, but argv is not a secret
channel either. Runtime overrides also depend on Codex's configuration contract;
explicit user overrides and administrator-managed settings can affect the result.
Routing applies to launches through the shim/wrapper, not arbitrary direct binary
or app launches. Repository hooks still require trust for funding capture; routing
does not bypass that approval or replace `notify`.

A possible future alternative is ingestion without a separate Authorization
credential: a Sentry-style ingestion URL containing an opaque project hash. This
could remove bearer credentials from both launch arguments and environment-based
integrations. It is a design option, not implemented behavior. The URL would still
be visible wherever endpoints are recorded. Its identifier should grant ingestion
only, with no read or administrative access, and support rotation and abuse controls;
if possession of the URL authorizes writes, it remains a capability rather than
being literally authentication-free.

Claude Code's settings carry no resource attributes. `OTEL_RESOURCE_ATTRIBUTES` is the
user's variable for describing their own resources, and nothing Terma used to put in it
is needed: the server key names the project, Claude Code stamps `user.id` and
`user.email` on every metric and event itself, and its resource already says
`service.name=claude-code`. The project a configuration reports to is recorded in the
connect journal instead, which is what `status`, `doctor` and key reuse read.

`terma connect claude --scope local` is the one place a repository file carries telemetry
settings: the committed `.claude/settings.json` gets the signal and content switches —
what to ship — and nothing about where or with which key. Claude Code applies project
settings over user settings, so the layer narrows the global connect inside that
repository and does nothing at all without one. Codex has no project layer, so the option
is Claude Code only.

## The shim, and why it chains `.git/hooks`

With `core.hooksPath=.terma/hooks`, `git rev-parse --git-path hooks` returns the shim
directory itself — the first version of the shim used it to find "the previous hook" and
exec'd itself forever. The shim now chains `${GIT_DIR:-.git}/hooks/<name>` (git runs
hooks from the worktree root), asks `git rev-parse --git-common-dir` only for linked
worktrees, and refuses to exec anything that is the same file as `$0`.
`TERMA_CHAIN_HOOKS_DIR` overrides the location for repositories that kept hooks elsewhere.

## Harness-agnostic

`internal/harness` is the only package that knows a harness's file layout. `hookrun`
speaks in events (session start/end, files touched, commit) and the tool label travels as
data. The dashboard shows which harness produced spend, but nothing in the pipeline is
special-cased on it: a new harness is a new adapter, not a new hook type. The
adapters are registered once, in `internal/adapter`, and install, uninstall, doctor and
hook dispatch all read that registry.

Not every harness offers a file to write environment variables into. OpenCode reads its
OTEL_* variables from the process environment only, so its adapter is a plugin: one
dependency-free JavaScript file Terma writes into OpenCode's plugins directory, which
turns OpenCode's own events into OTLP/JSON and calls `terma hook opencode-*` for
attribution. The harness interface is the same; only what Connect writes differs.

## Threat model in one line

Trailers, identities, and session events are attribution among colleagues, not
authentication — anyone with commit access can forge them. See `SECURITY.md`.
