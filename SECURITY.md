# Security model

What `terma` protects, from whom, and where the remaining edges are. Written to be argued
with — if a claim here is wrong, that is a bug.

## What is at stake

| Secret | Lifetime | Where it lives | If leaked |
|---|---|---|---|
| CLI access token | 1 h | `~/.config/terma/credentials.json` (0600), one per organization signed into | Read every project in the org until it expires or is revoked |
| CLI refresh token | 30 d, rotated per use | same file; hash only server-side | Mint access tokens until detected or revoked |
| Authorization code | 60 s, single use | never written to disk | Useless without the PKCE verifier |
| PKCE verifier | one login | process memory only | Useless without the code |
| Project server key `ter_srv_` | until revoked | `~/.config/terma/keys.json` (0600) and the harness's own config | Write telemetry into the one project it is bound to |

A CLI credential is **org-scoped**. A server key is **project-scoped and write-only for
telemetry**; it is what a harness exports with and what the spool delivers with. Neither
is ever written into a repository: `terma install` produces hook wiring and `.terma/settings.json`
(project id and hook installation metadata) in the repository. The agent hook files
themselves record which adapters are wired. Install also writes per-developer
keys and routing configuration under the user's configuration directory, may update
agent settings, and offers to add a managed PATH block to the shell startup file.

## The login flow

```
verifier (memory)          ──never leaves the process──┐
challenge = S256(verifier) ──through the browser──▶ app.terma.ai/cli/auth ──▶ gateway stores it with the code
code ──through the browser──▶ http://127.0.0.1:<port>/callback ──with the verifier──▶ auth.terma.ai
```

| Control | Attack it stops |
|---|---|
| PKCE S256, verifier never in the URL | A code read from history, a screenshare, or a proxy log is not redeemable |
| `state`, 128-bit, constant-time compared | Another page or local process injecting a code into your session |
| Code single-use, 60 s, bound to the loopback port | Replay; redemption by a listener other than the one that asked |
| Listener on `127.0.0.1` only | Anyone else on your network reaching the callback |
| `Host` header must name the loopback listener | DNS rebinding reading the response |
| A mismatched callback is ignored, not fatal | An attacker who guesses the port cancelling your login |
| Refresh rotation + reuse detection | A stolen refresh token outliving discovery |
| https required off loopback | A mistyped endpoint putting secrets on the wire in cleartext |
| The approval page validates the link's shape and builds the redirect itself | An open redirect through `/cli/auth` |

## Hooks: what they can and cannot do

- **Never fail a commit.** Every shim runs `terma hook … || true`; a missing or broken
  `terma` is ignored. `prepare-commit-msg` touches only the message file and files under
  `.git/terma/`; it makes no network call and has a 50 ms budget CI enforces.
- **Never block on the backend.** Hooks append to a local spool. Delivery is a detached
  background process with exponential backoff; the spool is bounded (16 MB, oldest
  dropped) so a dead backend cannot fill a disk.
- **Committed files are inert without the binary.** The shims, `.claude/settings.json`
  hooks, and `.terma/settings.json` contain no secrets and do nothing on a machine without
  `terma`.
- **Chaining is bounded.** A shim chains the repository's own `.git/hooks/<name>` (or
  `TERMA_CHAIN_HOOKS_DIR`), never `core.hooksPath`, so it cannot recurse into itself.
- **Hooks do not read what was said — with one exception.** Session ids, file paths, tool
  names, timings, plan and quota windows: that is what a hook reports. Prompts, tool input
  and output, error text and transcripts are not opened; content reaches Terma only through
  the agent's own export, under the capture switches you chose. The exception is Codex's
  replies, which its export leaves out entirely. At the end of a turn `terma hook
  codex-stop` reads the assistant messages of that turn from the rollout — through a root
  confined to `CODEX_HOME`, refusing symlinks and non-regular files, a bounded number of
  bytes and messages per invocation — and only when Codex already exports your prompts for
  this repository: the routing record and the machine-wide config both have to allow it
  where they exist, because a hook cannot tell which one started the session, and with
  neither there is nothing to extend. It fails closed — a configuration that is there and
  cannot be read might be the one that says no, so an unreadable one is a no. `--exclude-prompts` therefore withholds replies too,
  as its help text has always said. Until delivered the text sits in the spool
  (`~/.config/terma/spool`, 0600, 14 days at most) — the one place terma holds
  conversation content at rest, and another reason "anything running as your user" is
  outside what this protects against.
- **Every commit is counted, not catalogued.** `post-commit` spools an event for each
  commit so coverage can be measured. For a commit no agent session was stamped into,
  that event carries the commit's identity and size only — sha, remote URL with
  credentials stripped, branch, author email, file count, lines added and deleted — and
  never file names or per-file stats; those appear only on commits that carry a session
  trailer. Merge and squash commits produce nothing.

## Per-repository routing

`terma install` points each agent at the repository's project without putting secrets in
the repository. The per-project server key stays in the home directory
(`keys.json`, the keystore, or a 0700 headers-helper script), namespaced by project id;
the committed `.terma/settings.json` names only the project. A PATH shim (or shell
wrapper) resolves the real agent from PATH — skipping its own directory by file identity
so it can never exec itself — bounds Terma preparation to two seconds, reads a versioned,
data-only argument file (no `eval`), and execs the agent exactly once, passing preparation
through on any failure.

- **Claude Code** is routed with `--settings <file>` and a 0700 headers helper; the key
  is never written into the settings document.
- **OpenCode** routes itself from a committed binding through a per-project helper; the
  key lives only in that helper. The shared global plugin captures no prompt or tool
  content by default — a repository opts in through its own committed `.opencode/terma.json`,
  so one project's install can never widen another's capture.
- **Codex** receives per-launch telemetry through `-c` overrides, **including the bearer
  key** (`Authorization: Bearer …`). Codex's `[otel]` table has no way to reference the
  key indirectly, so it rides in the process arguments: **visible in `ps` /
  `/proc/<pid>/cmdline` to anything running as your user for the life of the session.**
  This is a deliberate, documented trade-off (see `docs/DESIGN.md`); Terma never logs the
  generated argv.
  The per-launch argument file is written under a `mktemp -d` 0700 directory at 0600.

## Identity and attribution are not authentication

The `Agent-Session-Id` / `Agent-Tool` trailers, the `enduser.id` a harness sends, and the
session events the spool delivers are **attribution among colleagues, not a security
boundary**. Anyone with commit access can write any trailer by hand; anyone can set any
identity in their harness config. Terma treats them as *who to bill the work to*, not as
proof of who did it. Outcomes that need to be trustworthy (what merged, what was reverted,
what CI said) come from the GitHub App, which reads the trailers back from commits it can
verify came through GitHub.

## What this does *not* protect against

- **Anything running as your user.** It can read `credentials.json` and `keys.json`.
  File permissions stop other users, not your own code. If you run untrusted code,
  `terma logout` and rotate the project key in the Terma app.
- **Root, or anyone who can read your disk.** Full-disk encryption is the control.
- **A local process reading Codex's arguments.** The Codex bearer key rides in `-c`
  arguments (see "Per-repository routing"); any process running as your user can read it
  from `ps` while a routed Codex session runs. It is a project-scoped, write-only
  telemetry key, revocable in the Terma app.
- **The shell startup file terma edits for PATH.** `terma install` appends one consented,
  marked block to your `~/.zshrc`/`~/.bashrc`/fish config to put the shim directory on
  PATH; the path is shell-escaped so it cannot inject, and `shim uninstall` restores the
  file. Anything that can already write your rc file can do more than this.
- **The one-hour window after offboarding.** A removed user's access token stays valid
  until it expires unless the session is revoked from the app.
- **A malicious browser extension.** It can watch you approve; it cannot redeem the code.
- **A hostile repository.** `terma install` writes files you review in a PR; but a
  repository that already carries a malicious `.terma/hooks/*` runs it like any other
  hook once `core.hooksPath` points there. `terma install` sets that only in a repository
  whose `.terma/settings.json` you have chosen to trust; review the shims like any other code.

## Reporting

Do not open a public issue for a vulnerability. Contact the Terma team at
security@terma.ai.

## For reviewers

Properties worth re-checking after any change:

- The verifier never appears in a URL, a log, or a file (`internal/auth` tests).
- No credential or key is rendered by `-o json` (`config show`, `status`, `doctor`).
- `prepare-commit-msg` makes no network call and stays under budget (`make bench-hook`).
- Human-only commits are never stamped; a session stamps only files it touched
  (`internal/session` and `internal/hookrun` tests).
- The event for an unstamped commit names no file and carries no credential
  (`internal/hookrun` `TestPostCommitOnAnUnstampedCommitEmitsOnlyACount`).
- The shim never chains itself (`internal/hookmgr` `TestShimNeverChainsItself`).
- Events without a project key are held, not dropped (`internal/spool` held-event test).
- The generated agent argv (which carries the Codex bearer key) is never logged
  (`internal/shim`, `internal/harness` — only the argument *count* is formatted).
- The PATH line written to a startup file is shell-escaped and inert for a directory
  with metacharacters (`internal/shim` `TestPathLineEscapesHostilePaths`).
- A per-repo Codex run turns unselected signals off, never inheriting another project's
  exporters (`internal/harness` `TestCodexRuntimeArgs`).
- No project id becomes a filesystem path or script name without validation
  (`internal/project` `ValidID`, mirrored in the OpenCode plugin).

`govulncheck` runs in CI on every push and weekly.
