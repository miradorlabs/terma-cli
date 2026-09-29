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

## Repo scope vs. machine scope

`terma install` writes only things that belong in the repository and carry no secrets:
hook wiring through the manager the repo already uses (husky, lefthook, pre-commit) or
committed shims plus `core.hooksPath`; `.claude/settings.json` project hooks; and
`.terma/settings.json` binding the repo to a project. One merged PR onboards everyone.

Telemetry is the machine's. `terma setup` writes each agent's global configuration once
— Claude Code's `~/.claude/settings.json`, Codex's `~/.codex/config.toml` — pointed at
terma's loopback relay, which sends each record to the project of the repository its
session ran in, or with `--no-relay` straight at Terma with the machine project's key.
`terma install` binds the repository, which is all the relay needs, and configures the
agents itself only when setup has not. Keys live in the home directory, namespaced by
project. Events spooled before a key is stored are **held**, not dropped, so ordering
loses no data.

### Decision: global configuration and a relay, not a launcher

Earlier versions routed per repository at launch: a PATH shim in front of `claude` and
`codex`, a block at the end of the shell's startup file to put it first, a per-project
Claude settings document handed over as `--settings`, and Codex telemetry passed as
per-launch `-c` overrides. It worked for a shell launch in a shell that had read the
startup file, and for nothing else. Codex Desktop and the ChatGPT app run one app server
for every folder; Claude Desktop starts its own Claude Code by absolute path; an IDE
extension does the same. None of them passes through a PATH shim, and a
per-process configuration cannot tell one repository's thread from another's in a
process that serves them all.

What they all do read is the agent's global configuration, and what every record they
export carries is its session (`session.id` for Claude Code, `conversation.id` on
Codex's logs, a trace id shared with them on its spans). So the global configuration
names one destination — the relay — and the relay decides the project per record, from
the session's directory as the session-start hook recorded it (or the rollout's and the
transcript's own record of it). See [RELAY.md](RELAY.md). The relay keeps the
credentials: no agent file holds a Terma key, and none names a project.

The cost is a service on the developer's machine (launchd, systemd --user) and records
without a session — Codex's metrics, its process-level spans — that can only go to the
machine project. Direct mode (`--no-relay`, or no service manager) is the fallback that
needs neither and gives up per-repository projects: every session reports to the
machine's. The launcher is gone; `terma shim prepare` still answers an old shim with an
empty plan, so the agent starts unchanged until setup, install or a refresh removes it.

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
