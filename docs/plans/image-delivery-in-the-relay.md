# Image delivery in the relay

Status: proposal (2026-11-12), for terma-cli #126. Nothing here is built yet.

## Why

Pasted prompt images are big: up to 2 MiB raw, about 2.8 MB on the wire. Today they leave
from short-lived detached processes (`terma spool flush --images`), each a pass with a deadline
(5 minutes). Seven review rounds have each fixed one consequence of that and found the next:

- a pass deadline an upload can outlive (acknowledging past it, deferring on it, checking time left
  before each image and before the bare resend after a 400/413);
- a queue appended to while a pass runs (`Drain` re-reading; `Grew` and a self-started extra pass,
  which the last rating found loops forever on a read-only disk);
- deciding from outside whether a pass is worth starting (`ImagesDue`, `held.json` holds, `NoWait`,
  `Busy`), and holds outliving the key they waited for (`terma setup`);
- rotating projects across processes (`Mark` on disk).

None of that is about images. It is about where they are sent from. The relay
(`terma relay run`, `internal/relay/daemon`) already runs the whole time on `127.0.0.1`, already
sends each team's records with the team's key, and has no pass deadline: a 2.8 MB upload on a
slow link simply takes as long as it takes.

## Proposal

The relay sends the image queue. Hooks keep doing what they do now: read the session file, keep
the bytes in `<state>/images/<sha256>`, append one `terma.prompt.image` record to
`<state>/image-spool/`. They stop starting `--images` passes.

**One image sender in the relay**, a `daemon.Config.Workers` entry wired from `cli`
(`cli/relay_run.go`, beside `PolicyRefresher` and `sweepHookState`). It is a loop that sleeps until
woken, then delivers what the image queue holds, one image at a time:

- Placement, policy, consent and the endpoint go through the same `delivery.Router` the events'
  flush uses (`places`, `Outgoing`, `Allowed`, `senderFor`, `Endpoint`). The relay process already
  builds one through `cli`.
- Sending and judging an image move unchanged: `sendImage`, `withImageData`, `judgeImage`
  (`hostUnavailable`, `uploadBroke`), the outcome table `imageRules`, per-record `Settle`,
  per-project retry windows in `image-spool/retry.json` (which doctor reads), `forgetImages`.
- Each request keeps its own deadline, sized by its body (`timeoutFor`). There is no pass deadline.
- Projects take turns in memory (a cursor in the loop), not through a file.

**What wakes it** (event-driven; no polling of the queue):

1. **The agent's own telemetry for the turn.** Claude Code's Stop hook queues an image moments
   before Claude Code exports that turn's logs (every 5 s, and on shutdown); Codex sends its turn's
   spans at turn end. Each OTLP request the relay accepts nudges the sender (a non-blocking send on
   a channel). The sender then reads the queue's size and does nothing if it hasn't grown and no
   window has closed. The cost per export is one `stat`.
2. **Start.** The relay drains the queue once when it starts. A hook that queues an image already
   reaches `daemon.Spawn` (emitting an image claims the session), so a relay that wasn't running is
   started and drains on start.
3. **A retry window closing.** After a failure the sender knows when each project may next be tried;
   it sleeps until the earliest such time (a timer on a known deadline, a safety net, not a poll).

**What goes away** (in `delivery`, `spool`, `cli`, `hookrun`, `dispatch` and the agents):
`ImagePassTimeout`, `DrainOptions.Timeout`, `errNoTimeLeft`/`timeLeft`, "cut off by the pass" as an
outcome, `Result.Grew`/`drainPass.unseen` and the re-spawn, `NoWait`/`SkipBusy`/`Spool.Busy`,
`held.json` (`Hold`/`Unhold`/`Holds`) and `ImagesDue`, `Mark`/`SetMark`, `spawnImageFlush`,
`Env.FlushImages` and the agents' calls to it. Roughly half of the lane's code.

**What stays outside the relay:**

- `terma spool flush --images` stays as a manual command (and for doctor): it delivers the image
  queue once in the foreground, taking the image queue's delivery lock, so it never runs alongside the
  relay's sender (which takes the same lock per image).
- The events' flush keeps moving image records an older build left in the events queue over to the
  image queue.

## Decisions for the user

1. **The wake signal.** The recommendation above uses the agent's telemetry, which is already
   arriving. Its gap: a session whose agent exports no telemetry to the relay (an exporter turned off
   by the user or an enterprise policy) gets its images only when the relay next starts or another
   session's telemetry arrives. The alternative is a direct nudge from the hook: `cli` (not the
   offline hook packages) posts to a new token-authenticated `POST /wake` on the relay, beside
   `/heartbeat`. That is exact, but it is a hook making a loopback HTTP request, which reads against
   "hooks never touch the network". A third option, a 1 s check of the queue's size on the relay's
   existing 1 s tick, breaks "never poll". I recommend the telemetry nudge, plus start and windows.
2. **No relay.** Without the per-user service (opted out, unsupported OS), the relay is started by a
   claiming hook and exits after 8 h idle. Images then go while it runs, and on its next start. That
   matches how the relay already treats agents' telemetry.

## Risks and how they are handled

- **Shutdown.** `daemon.Run` waits for its workers with no bound (`run.go`, `workers.Wait()`).
  The sender must return promptly when its context ends: it cancels an upload in flight (the record
  stays queued, settled only on a 2xx), so a replace, an update or `terma teardown` never waits on an
  upload. Add a bound to `workers.Wait()` in the same change.
- **Idle exit and updates.** `Idle()`/`Quiesce()` ignore workers today, so an 8 h idle exit could cut
  an upload. The sender reports busy while uploading, and `Idle()` counts it.
- **Two senders.** The relay's sender and a manual `--images` flush take the image queue's delivery
  lock per image, so they never send the same record twice.
- **Boundaries.** `internal/relay/daemon` may import `delivery`, `spool` and `promptimage` (no
  boundary rule bans them; no import cycle). Wiring from `cli` is simplest and keeps the engine
  (`internal/relay`) free of `routing`.
- **Doctor.** It keeps reading `image-spool/retry.json`. Its fix text stops saying doctor started a
  pass; it says the relay will retry at the window's end.

## Order of work

1. Add the sender as a relay worker, with the three wakes and the busy/idle fix; keep the detached
   lane in place behind it.
2. Point hooks away from `--images`; keep `--images` as the manual command.
3. Delete the pass machinery listed above, and its tests; move the send/judge/settle tests to the
   sender.
4. Re-rate.
