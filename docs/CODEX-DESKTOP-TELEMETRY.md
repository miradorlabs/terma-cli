# Codex Desktop telemetry

Codex Desktop (and the Codex inside the ChatGPT app) runs one long-lived app server for
every thread in every folder. Nothing per launch reaches it — which is why the PATH shim
earlier versions of terma put in front of the Codex CLI never covered it — but it reads
the same user-level `$CODEX_HOME/config.toml` as the CLI. So `terma setup` configures it
there, once, with the CLI: Codex's native OpenTelemetry export, pointed at terma's relay
(which sends each thread to its repository's project, by the thread's `conversation.id`)
or, with `--no-relay`, straight at Terma with the machine project's key. See
[the relay](RELAY.md).

## Set up

1. Select **Codex Desktop** in `terma setup` (select **Codex CLI** too if you use its
   shell command). Setup configures Codex's global `config.toml`.
2. Run `terma install` in each repository you work on. It binds the project and writes
   the Codex hooks (`.codex/hooks.json`) for you to commit: they announce each session,
   so commits are stamped with it and the relay knows the thread's repository at once.
3. Approve the hooks in Codex Desktop:
   1. Open this repository in Codex Desktop and trust the project if prompted.
   2. Open **Settings → Hooks** and select **Review** for the entries from this
      repository's `.codex/hooks.json`. If hooks are disabled, enable them there.
   3. Inspect the `terma hook …` commands and approve each Terma entry. Codex skips any
      entry you leave untrusted.
4. Run `terma desktop status` in the repository: it reports Codex's export (through the
   relay, or straight to Terma), the relay's state, and whether the hooks are trusted.
   Restart Codex Desktop after setup so the app server reads the new configuration, then
   run `terma doctor` to verify delivery.

Codex requires review again whenever a hook definition changes. `terma install` does
not approve hooks on the user's behalf.

## What is captured

Codex's native export: its logs (`codex.*` events, each carrying `conversation.id`),
traces and metrics, as the Codex CLI sends them. The relay routes a thread's logs by
`conversation.id` and its spans by trace id; Codex's metrics carry no session and go to
the machine project. Terma's hooks add what Codex does not export — session starts,
files touched, the assistant's replies and the thread's title under prompt consent, and
plan quota from the rollout — delivered through the local spool with the repository's
project key.

Earlier versions captured Desktop through repository hooks alone, reading tool calls,
turn timing and compaction from the rollout (`terma.turn.summary`, `terma.compaction`,
`terma.approval.requested`), because Desktop had no native export of its own. That
capture ran only for a repository with a Desktop routing record; terma no longer writes
those records, and removes them, so Desktop's native export is its source now.

For Codex-managed local worktrees, a Git-ignored `.terma/settings.json` must be copied
into the worktree. Add it to `.worktreeinclude` when needed.

See the [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference),
[hooks guide](https://learn.chatgpt.com/docs/hooks), and
[worktree guide](https://learn.chatgpt.com/docs/environments/git-worktrees).
