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
sessions), and pointing each agent at *this repository's* project through the local relay
(below). Keys live in the home directory, namespaced by project. `terma
setup` is the optional machine-level preamble: sign in and record which agents you use.
Events spooled before a key is stored are **held**, not dropped, so ordering loses no
data.

### Decision: route through a local relay, not a launcher

Per-repository routing used to wrap the agents' launch: PATH shims ahead of the real
`claude` and `codex`, handing Claude a per-project `--settings` document and Codex `-c`
overrides. It could not reach what does not start from a shell (Claude Desktop, Codex
Desktop, IDE extensions, an app started from the Dock), both agents refuse
repository-level exporter settings, and every launch depended on each agent's command
line contract. It is removed; `terma update --refresh` takes the shims off machines that
have them.

Instead every agent exports from its user-level settings — which every surface reads —
to a relay on the developer's machine, and the relay forwards a session only when a hook
in an installed repository claimed it, to that repository's project, with its key and
under its content policy. Everything else is held briefly in memory and dropped; nothing
unclaimed goes on the wire. Agents without a usable exporter get one terma writes into
them (OpenCode, omp, Pi, Hermes, DeepSeek Harness). The design, what it catches and misses,
and every attempt to break it are in [RELAY-SPIKE.md](RELAY-SPIKE.md).

The costs: a process on the developer's machine (started by hooks, or a per-user service,
which macOS announces as a background item), and exporter settings that are machine-wide
— correct only because the relay, not the exporter, decides what leaves.

### Relay package boundaries

`internal/relay` owns OTLP admission, attribution, filtering and delivery. Its focused
subpackages keep the other responsibilities out of command handlers:

- `claim`: the small, local session-claim store that hooks can import without OTLP.
- `exporter`: the `Exporter` interface and registry for machine-level exporter setup.
  Native exporters and extension exporters implement the same configuration operation.
  The registry also owns hook-label normalization and surface selection, so adding an
  exporter does not require another agent-name switch in `cmd`.
- `service`: rendering launchd, systemd and Windows service definitions. The CLI owns
  service-manager execution, relay startup and user-facing reporting.

Repository routing records narrow team capture with their saved harness list as well
as signals and content. A claimed session from an unselected harness is withheld on
admission and queued delivery. Global catch-all delivery has no harness claim and
continues to follow the selected team's global policy. Codex's CLI and Desktop share
one native exporter; the separate surface flags still control hook capture and setup.

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

Commands ask for capabilities rather than checking an agent's name: `adapter.UserHooks`
and `adapter.ManagedHooks` handle machine-wide hook planning and locations;
`harness.StatusLiner` and `harness.TurnNotifier` handle optional usage capture on connect
and cleanup on disconnect. Every implementation has a compile-time interface assertion.
Keep vendor file layouts and behavior in the implementation, and keep command handlers
responsible for selection, confirmation and reporting.

Not every harness offers a file to write environment variables into. OpenCode reads its
OTEL_* variables from the process environment only, so its adapter is a plugin: one
dependency-free JavaScript file Terma writes into OpenCode's plugins directory, which
turns OpenCode's own events into OTLP/JSON and calls `terma hook opencode-*` for
attribution. The harness interface is the same; only what Connect writes differs.

## Threat model in one line

Trailers, identities, and session events are attribution among colleagues, not
authentication — anyone with commit access can forge them. See `SECURITY.md`.
