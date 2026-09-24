# Codex Desktop: repository-specific telemetry

Codex Desktop does not launch through the shell shim used by Codex CLI, and Codex
ignores `otel` in repository config. Terma captures Desktop activity with trusted
repository hooks and its existing local spool. It installs no background receiver,
LaunchAgent, or global Codex exporter.

## Set up

1. Select **Codex Desktop** in `terma setup`, or run `terma install --harness
   codex-desktop` in a repository. Select **Codex** too if you use its shell CLI.
2. Run `terma install` in each repository you want to report. It binds the
   project, stores a project key and local content policy, and writes the Codex
   hooks for you to commit. Setup alone writes no repository files.
3. Approve the hooks in Codex Desktop:
   1. Open this repository in Codex Desktop and trust the project if prompted.
   2. Open **Settings → Hooks** and select **Review** for the entries from this
      repository's `.codex/hooks.json`. If hooks are disabled, enable them there.
   3. Inspect the `terma hook …` commands and approve each Terma entry
      for full capture. Codex skips any entry you leave untrusted.
4. Run `terma desktop status` in the repository. **Codex hooks: ready** confirms
   that the current definitions are trusted. Then start a new **Local** task in
   that repository and send a test message. Run `terma doctor` to verify
   delivery. Codex CLI is not required; its `/hooks` command is an alternative
   review path for CLI users.

Codex requires review again whenever a hook definition changes. `terma install`
does not approve hooks on the user's behalf.

An install that finds Terma's earlier Desktop relay exporter removes that
exporter and its LaunchAgent. Restart Codex Desktop once so its backend unloads
the old exporter. `terma desktop disconnect` can remove the old relay without
reinstalling. An unrelated user-level exporter is left alone.

## What is captured

- `SessionStart` marks the session. `UserPromptSubmit` reports the turn and
  sends prompt text only if this repository allows it.
- `PreToolUse` and `PostToolUse` pair by Codex's tool call ID to report an
  observed elapsed time. It includes approval waits and hook scheduling, so it
  is not Codex's native execution duration. `PostToolUse` reports local tool
  calls and results; arguments and output travel only when this repository
  allows tool content. Identified file edits carry the same call ID.
- `PermissionRequest` records that Codex asked for approval, and an optional
  reason under the tool-content policy. Codex does not report the user's answer
  to a repository hook. Terma labels this **requested**, never approved/denied.
- `Stop`, `SessionEnd`, and `PostToolUse` read a bounded local Codex rollout
  cursor for per-response token usage, completed hosted Extension actions,
  turn duration and time to first token, and compaction duration where present.
  The rollout's trace ID is carried as a cross-reference when available; its
  absence does not create a synthetic native span. Assistant replies are read
  at turn end only if this repository allows prompt content. These readers
  append to the spool before advancing their cursors.
- The existing spool delivers events with this repository's project key. A
  missing key holds them locally for later delivery.

The rollout is a private, unstable Codex format. Terma reads only the specific
record shapes above, with a 1 MiB and 128 relevant-record limit per invocation;
later hooks continue a backlog. Codex's published hooks also exempt some hosted
and specialized tools. The Extension reader covers the observed hosted action
shape, but **there is no stable Codex API that guarantees every Desktop tool
call**. New Codex item types require a Terma update. Per-request SSE/WebSocket
timing, actual user approval decisions and their source, and native trace spans
still require Codex's native OTel export. Codex ignores `otel` in project config,
so that export is machine-wide unless Codex adds a scoped Desktop interface.

Codex CLI launches routed by the Terma shim keep their native OTLP configuration
and are not converted to Desktop hook telemetry. A separate, unrelated global
Codex exporter can still export machine-wide data under its own configuration;
`terma install` does not manage it. `terma desktop status` reports whether one
is active. If you require exports only from installed repositories, disconnect
that user-level exporter (use `terma disconnect codex` for a Terma-owned one).

For Codex-managed local worktrees, a Git-ignored `.terma/settings.json` must be
copied into the worktree. Add it to `.worktreeinclude` when needed.

See the [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference),
[hooks guide](https://learn.chatgpt.com/docs/hooks), and
[worktree guide](https://learn.chatgpt.com/docs/environments/git-worktrees).
