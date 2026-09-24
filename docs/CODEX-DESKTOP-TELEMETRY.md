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
3. Open the repository in Codex Desktop. Review and trust Terma's hooks in the
   app's **Hooks** settings or its in-app trust review when prompted. Codex
   requires this for each new or changed hook definition; `terma install`
   cannot approve hooks on a user's behalf. You do not need Codex CLI. If you
   also use the CLI, `/hooks` offers the same review there.
4. Start a new task in that repository. Run `terma desktop status` to inspect
   the route, key, content choices, and hook trust. Run `terma doctor` to verify
   delivery.

An install that finds Terma's earlier Desktop relay exporter removes that
exporter and its LaunchAgent. Restart Codex Desktop once so its backend unloads
the old exporter. `terma desktop disconnect` can remove the old relay without
reinstalling. An unrelated user-level exporter is left alone.

## What is captured

- `SessionStart` marks the session. `UserPromptSubmit` reports the turn and
  sends prompt text only if this repository allows it.
- `PostToolUse` reports local tool calls and results. Arguments and output
  travel only when this repository allows tool content. Identified file edits
  carry the same tool call ID, so the platform folds them into one call.
- `Stop`, `SessionEnd`, and `PostToolUse` read a bounded local Codex rollout
  cursor for per-response token usage and completed hosted Extension actions,
  which do not arrive through ordinary tool hooks. Assistant replies are read
  at turn end only if this repository allows prompt content. These readers
  append to the spool before advancing their cursors.
- The existing spool delivers events with this repository's project key. A
  missing key holds them locally for later delivery.

The rollout is a private, unstable Codex format. Terma reads only the specific
record shapes above, with a 1 MiB and 128 relevant-record limit per invocation;
later hooks continue a backlog. Codex's published hooks also exempt some hosted
and specialized tools. The Extension reader covers the observed hosted action
shape, but **there is no stable Codex API that guarantees every Desktop tool
call**. New Codex item types require a Terma update.

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
