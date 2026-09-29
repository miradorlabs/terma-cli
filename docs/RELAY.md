# Terma relay

Every coding agent's native OpenTelemetry export is configured once, machine-wide, and
points at a relay terma runs on loopback. The relay decides, record by record, which
project a record belongs to, and delivers it with that project's key. It replaces the
per-launch shims: Codex Desktop, the ChatGPT app's Codex, Claude Desktop and any other
launcher that bypasses a PATH shim are covered, because they all read the same global
configuration.

```
Claude Code ─┐                    ┌─ inbox/ ─ router ─ outbox/<project>/ ─ forwarder ─→ OTLP gateway
Codex CLI   ─┼─ OTLP/JSON ─→ relay┤                                      (project key)
Codex app   ─┘   127.0.0.1        └─ sessions/<id>  ← hooks (SessionStart)
```

## Routing

A record's project is decided in this order:

1. **Its session.** The session id is read from the record:
   `session.id` (Claude Code, every signal), `conversation.id` (Codex logs), a UUID-shaped
   `thread.id` / `thread_id` (Codex spans; most Codex spans carry a worker-thread number
   there instead, so the relay also maps each trace id to the session any record in that
   trace named).
2. **The session's directory**, from, in order: the hook index (`sessions/<id>`, written
   by a session-start hook), Codex's rollout header (`session_meta.cwd`), Claude Code's
   transcript directory name matched against the repositories this machine has
   installed.
3. **The directory's binding** (`.terma/settings.json`, through `project.ResolveDir`, so a
   linked worktree takes its main checkout's).
4. **Otherwise the machine project** chosen in `terma setup`: an unbound repository, a
   session the relay could not place within the hold window, and records with no session
   at all (every Codex metric; Codex process-level spans such as startup and auth).

A session's project is decided once and cached; it never moves mid-session.

Live-verified 2026-09-29 (Codex CLI 0.158.0, Claude Code 2.1.284, OTLP/JSON):

| | Codex | Claude Code |
|---|---|---|
| Logs | `conversation.id` on all but the first and last record | `session.id` on every record |
| Spans | UUID `thread.id` on a few; 88% reachable by trace id, the rest are process spans | `session.id` on every span |
| Metrics | no session id | `session.id` on every data point |

## Durability

- **Intake** writes the request body to `inbox/` before answering 200. Nothing is held
  only in memory, so a crash or restart loses nothing that was acknowledged.
- **The router** takes inbox files in order, splits each one per project and writes
  ready-to-send OTLP/JSON bodies to `outbox/<project>/`, then removes the inbox file.
  A request with a record whose session cannot be placed yet stays in the inbox and is
  retried until the hold window (30 s) closes; then its unplaced records go to the
  machine project.
- **One forwarder per project** sends the outbox oldest first. 2xx removes the file.
  Network errors, 408, 429 and 5xx back off exponentially (1 s to 5 min). Other 4xx
  (a refused key, a malformed body) move the file to `dead/` and are counted. A project
  with no key on this machine is held, not dropped.
- **Bounds:** 16 MiB per request, 256 MiB on disk across inbox and outbox (oldest dropped,
  counted), 14 days of age.

Request bodies that are not OTLP/JSON (protobuf) cannot be split and go to the machine
project whole.

## Security

The relay binds `127.0.0.1` only and requires `Authorization: Bearer <token>`; the token is
random, per machine, in `relay/relay.json` (0600). It is not a Terma credential: project
keys never leave the keystore, and no agent configuration holds one.

## Service

- macOS: a launchd agent, `~/Library/LaunchAgents/ai.terma.relay.plist`, `RunAtLoad` and
  `KeepAlive`, running `terma relay serve`.
- Linux: a systemd user unit, `~/.config/systemd/user/terma-relay.service`,
  `Restart=always`.
- Elsewhere, or with `terma setup --no-relay`: no relay; agents export straight to Terma
  with the machine project's key.

`terma update --refresh` restarts the service so the new binary serves. `terma relay
status` reports health, counts, backlog and the last delivery error; `doctor` and `status`
include it.

## State (`~/.config/terma/relay/`)

| Path | Written by | Contents |
|---|---|---|
| `relay.json` | `setup` | port, token |
| `sessions/<id>` | hooks | the session's directory (pruned after 30 days) |
| `inbox/` | intake | accepted request bodies awaiting routing |
| `outbox/<project>/` | router | per-project bodies awaiting delivery |
| `dead/` | forwarder | refused bodies (bounded) |
| `stats.json` | relay | counters and last error, for `status` |
