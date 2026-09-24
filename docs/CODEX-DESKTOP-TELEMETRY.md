# Codex desktop: repository-specific telemetry

Codex desktop starts its own backend, so it does not pass through the `codex` PATH
shim that `terma install` uses for CLI sessions. Codex also ignores `otel` in a
repository's `.codex/config.toml`. Desktop export therefore needs one user-level
logs exporter and a local receiver that determines each log record's project.

## Set up

1. Select **Codex Desktop** in `terma setup`, or pass
   `--harness codex-desktop` to `terma install` for one installation. If you
   also use the `codex` shell command, select **Codex** as well. Setup only
   saves your choices; it does not configure telemetry.
2. Run `terma install` in each repository whose desktop sessions should report to
   Terma. It creates the repository binding, a Codex route and key, and the
   `.codex/hooks.json` hooks. It also installs a loopback receiver as a macOS
   LaunchAgent and sets Codex's user-level logs exporter to
   `http://127.0.0.1:43199/v1/logs`. The receiver keeps a private on-disk queue
   and looks up each destination key at delivery time. On another platform,
   or with `terma install --desktop-manual`, run `terma desktop serve` separately.
   Open the repository in Codex and review its hooks with `/hooks`; the
   `SessionStart` hook must be trusted for routing to work.
3. Restart the Codex desktop app. From an installed repository, run
   `terma desktop status` to check the exporter, receiver, project route, and
   Codex hook trust. Start a new desktop task in that repository, then check the
   receiver's accepted-record count and the project's sessions in Terma.

`terma desktop connect` remains available to configure the receiver directly
without re-running repository install. `terma desktop disconnect` restores the previous Codex exporter settings that
Terma owns and removes its LaunchAgent. Restart the desktop app afterward.

## Routing and limits

The receiver accepts OTLP logs on loopback. A trusted repository `SessionStart`
hook registers its Codex `session_id` against the repository's project binding.
The receiver sends only records whose `conversation.id` matches a registration
and whose project still has a Codex logs route and key. A mixed batch is split
before delivery. Records without a matching session stay local and are counted
as unrouted; they are never assigned by the current working directory or a
machine-wide default project. A short wait handles records exported just before
the session hook completes.

The desktop choice is stored in each project's local routing record. A new
`terma install` run that selects Codex CLI but not Codex Desktop leaves desktop
delivery off for that project. Existing routing records created before the
desktop choice was available keep their previous behavior until reinstalled.
Desktop routing requires the `logs` signal; `terma install --signals traces`
refuses a desktop selection.

The desktop connection sends logs only. Codex's native aggregate metrics have
no reliable conversation ID for repository routing, so the receiver does not
accept or forward them. Terma's ingest derives usage and cost observations from
the routed Codex log events. Native traces are also outside this route.

Codex's user-level exporter must allow prompt text and tool output so that a
repository can opt in to either one. It sends them only to the local relay. The
relay drops `codex.user_prompt` and `codex.tool_result` records when the
repository route excludes those content types, and drops every record from an
unregistered session. Codex can include tool arguments even with tool output
capped at zero, so the relay drops the whole result when tool content is excluded.
The repository's prompt policy also controls assistant-reply capture from the
local rollout. CLI launches retain their own runtime `-c` route and are
distinguished from desktop launches by a process marker. A newly enabled prompt
setting applies after restarting Codex; prompts already exported as redacted
cannot be recovered from Terma's events.

The queue is under `~/.config/terma/desktop-relay/` (or `TERMA_CONFIG_DIR`),
with a 64 MiB bound and 14-day item lifetime. A full queue causes the receiver
to reject the batch so Codex can retry. A missing or changed project route
leaves its queued batches local until the route is repaired.

For a Codex-managed local worktree, Git-ignored `.terma/settings.json` must be
copied into the worktree. Add that path to the repository's `.worktreeinclude`
if the binding is ignored. Ordinary Git worktrees do not use this mechanism;
install or copy the binding into those worktrees yourself.

See the [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference),
[hooks guide](https://learn.chatgpt.com/docs/hooks), and
[worktree guide](https://learn.chatgpt.com/docs/environments/git-worktrees)
for the Codex behavior this design relies on.
