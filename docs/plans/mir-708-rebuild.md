# MIR-708: Rebuild Multi-Org Routing (#140) on the Single User Token

Status: Approved; Phase 2 implementation in progress.  
Branch stack: `dawson/mir-708-1-routing-cas` .. `dawson/mir-708-12-*` (branched from `origin/main` at `c0b2cb59`).  
Reference PR: `miradorlabs/terma-cli#140` (`dawson/multi-org-integration`, +29.5k / −1.6k across 244 files, ~14.5k non-test lines).

## Context

PR #140 implemented multi-org routing ("terma teams", bindings, crossing detection / Mixed runs, per-team policy refresh, "terma route explain", and doctor checks) on top of per-organization sign-ins, minted per-team keys (`keys.v2.json`), a one-approval-per-org browser flow, and a phased v1→v2 machine migration.

Since #140 branched from `main`, three major changes merged:
- **#137** — routing core (`internal/routing/`: `State`, `Team`, `Bindings`, `Router.Route`, `OrganizationOf`, `ParseEntry` + `entry_grammar.json`).
- **#150 & #152 (MIR-702 / MIR-703)** — single user token (`user-credentials.json`, one `gh`-style sign-in across every organization), OTLP delivery with `Authorization: Bearer <user-token>` + `X-Mirador-Project: <team_id>`, account-bound lanes (`account:<16-hex-key>` or `no_person` for server-key profiles), atomic per-session ownership (`internal/session/owner.go`), immutable `Manifest.Account`, `losses.json` / `spool/losses/`, and `-v2` queue isolation (`events-v2.jsonl`, `pushes-v2/`, `outbox-v2/`, `.git/terma/{events,pushes,losses}-v2`).
- **#155 & #157** — OMP native OTLP support via `hookrun.Extension`.

**Constraints for this rebuild:**
- Routing, bindings, crossing detection, and "terma teams" stay; the per-organization credential layer and per-team minted keys go.
- `ORG_NOT_ACCESSIBLE` becomes a per-request HTTP 403 reason (instead of a 401 on a per-org token refresh).
- **No 0.3.x backward compatibility or client migration** (Daniel's ruling): everyone signs in again at rollout. Drop #140's v1→v2 migration and do not build a new migration.
- Leave #144 (`dawson/bind-all`, `wip/bind-all-round6`) untouched; it will be rebased later.
- Ship as a stack of small, focused PRs, **every PR strictly under 1,400 lines of non-test (production) code**, leaving `main` safe to ship at every step and shipping invariant tests alongside each PR that introduces the behavior.

---

## 1. Piece-by-Piece Disposition of PR #140

| Ticket | Piece (packages / files in #140) | Action | Why |
|---|---|---|---|
| **MIR-684** | Machine migration & opt-in (#140: `migrate/`, `config/migrated.go`, `routing/migration.go`, `cli/relay_migrate.go`, `cli/teams_migrate.go`, `relay/daemon/migrate.go`, `semconv` `migration-unfinished`) | **Drop** | 0.3.x backward compatibility and legacy client migration are out of scope; `main` already isolates queues via `-v2` paths and retires legacy sessions on sign-in. |
| **MIR-684** | Routing state & store (`internal/routing/`: `recheck.go`, `state.go`, `store.go`) | **Adapt** | Keep `Recheck`, `RefusedAt`, `Team`, `SameGeneration`, `NewEnrollmentID`, and `OrgAccessLost`; drop `Migration`/`Preparing`/`Ready()`/`Legacy`; add `State.Account`, CAS `Expect{Account, Generation}` on `Update`/`Replace`/`Delete`, ordered `Epoch++` on every state-changing refresh (including success), and org-wide `ApplyOrgAccessLost`. |
| **MIR-685** | Run ledger (`runs/`, `boundary/runs_test.go`) | **Keep as is** | Pure local ledger of run locations, sidecar Mixed records, `Locational` exemptions, and queue-aware activity-based pruning (`runs/prune.go`); zero credential dependency. |
| **MIR-685** | Location-only relay claims (`internal/relay/claim/`: `at.go`, `claim.go`) | **Adapt** | Rename existing `Claim.Profile bool` → `Claim.CatchAll bool` first to avoid collision, then add `Kind`, `Run`, and `Profile string` while preserving #152's `Account` lane on `Claim` and `Placement`. |
| **MIR-685** | Spool destination metadata & session scan (`internal/spool/spool.go`) | **Adapt** | Keep `Profile`, `Enrollment`, `HoldKey`, `Run`, `OwnLocation`, and `Sessions()`; drop `DeliveryLockFile` export (migration-only) and keep #152's `events-v2.jsonl`, `Account`/`NoPerson`, and `spool/losses/`. |
| **MIR-685** | Routed hooks, placements, Codex per-turn routing, git hooks via `Route` (`internal/hooks/hookrun/{routing,placement,turns,commit,push,lifecycle,hookrun,relay}.go`, `dispatch/`, `repohooks/`, `agents/{claude,codex}`, `cli/hook.go`) | **Adapt** | Keep routed placement, `StartRun`, Mixed ledger updates, and `Route`-based git-hook install/stamping; preserve #152's lane checks, `session.Owner`, and sandbox parking (dropping/counting destination-less sandbox records on unpark); add `RunInfo.Routing` skew defense; wire `StartRun` + read/shell reach into all 5 `hookrun.Extension` agents before activation. |
| **MIR-686** | Structural shell reach & read/search crossing detection (`internal/hooks/hookrun/{reach,firstwords,shell*}.go`, `agents/internal/reach/`, agent read/shell hooks, Claude `hookmgr` upgrade refresh) | **Keep & fix** | Pure local crossing detection with zero credential dependency; fold in all open findings from PR #140's advisor review (cached folder operand, `find`/`fd` `-exec`/`-x` depth, enclosing checkout/`$HOME`, Glob `~`, `{ ... } \| bash`, `bash < file`, `bash -o pipefail`, non-blocking FIFO, composition fuzz property, and reach-hook latency test). |
| **MIR-687** | Policy team refresh & refusal classification (`internal/policy/` `team.go`, `internal/account/api/errors.go`) | **Adapt** | Keep `FetchTeam`, `RefreshTeam`, `Classify`, and add `api.ReasonProjectNotFound`; drop `api.RefreshError`, `LoadCredentialFor`, and `keys.v2.json`; fetch with the single user token (or `keys.json` on server-key profiles) and classify per-request HTTP 403 `ORG_NOT_ACCESSIBLE` as `OrgAccessLost`. |
| **MIR-687** | Relay routing, splitting, outbox, senders, daemon refresher & pruner (`internal/relay/`, `internal/relay/daemon/`, `cli/relay_teams.go`) | **Adapt** | Keep `Router`/`Rechecker`, batch splitting (upgraded to interval `[start, end]` Mixed checks), destination-pinned holds (fixing #140's re-routing in `hold.go`), fenced pre-POST send admission, distinct `outbox-v2/.teams/@<account>/<enrollment>/<tool>/` layout, queue-aware `runs.Prune`, and 1m/24h CAS refresher with coalesced ingest-403 nudge & backoff reset; drop `teammint.go`, `createTeamKey`, `keystore.DeleteTeam`, and dual-mode legacy paths. |
| **MIR-688** | Routed spool delivery (`delivery/routed.go`, `delivery.go`, `internal/cli/spool.go`) | **Adapt** | Keep per-enrollment `Recheck`, run-ledger Mixed + `runs.Locational` check, `HoldKey` check, and retry-by-enrollment; drop `Migration.Legacy`, `spoolkey.go`, `keys.v2.json`, and `EnrolledUser`; enforce lane gate first, drop/count destination-less events at activation, send via `sendas.SignIn` + `X-Mirador-Project` (or `keys.json` with no `X-Mirador-Project` for `no_person`). |
| **MIR-689** | `terma setup`, "terma teams", "terma route explain", `terma doctor`, `terma teardown`, remove `terma switch` (`internal/cli/`, `doctor/teams.go`, `internal/setup/setup.go`) | **Adapt** | Move `setup`/`teardown` and removal of `switch` onto `routing.State` BEFORE "terma teams" lands; fail closed on missing/corrupt/wrong-account state; drop `--authorize`, per-org `--org` sign-in, `SignedIn: false` rows, key minting/display, and migration checks; CAS-guard every `teams` write; merge #152 loss checks into `doctor` with single `terma setup` sign-in remediation. |
| **MIR-693** | One-approval multi-org browser sign-in & v2 credential/key indexes (#140: `account/auth/{index,migrate,login,store,keychain}.go`, `account/keystore/{teams,migrate}.go`, `cli/signin_browser.go`, `cli/teams_authorize.go`) | **Drop** | Completely replaced by #150/#152's single user token (`user-credentials.json`), which already reads every organization the user belongs to without per-org approvals or minted team keys. |

---

## 2. Bindings, Team Selection, and CAS State Management on One User Token

### What replaces per-org sign-ins and per-team keys

- **Single user token (`user-credentials.json` + keychain):** One sign-in per profile authenticates every API and ingest call across every organization the person belongs to:
  - List organizations: `GET /v1/organizations` (already on `main`).
  - List teams in an org: `GET /v1/projects?organization_id=<org_id>` (already on `main` in `internal/cli/project.go`).
  - Fetch a team's policy: `GET /v1/policy?project_id=<team_id>` (already on `main` in `internal/account/api/policy.go`, including 2.x wildcard parsing).
  - Send telemetry (relay & spool delivery) and heartbeats: `Authorization: Bearer <user-access-token>` + `X-Mirador-Project: <team_id>`.
- **No per-team minted keys (`keys.v2.json` is dropped):** Ticking a team creates no server key and writes nothing to the keystore; unticking or refusing a team deletes no key.
- **Server-key profiles (`TERMA_API_KEY`):** Represented as a **one-team `routing.State` with `Account: "no_person"`**. Setup validates the server key's team and environment (`ServerKeyAuthURL`), stores the key in `keys.json` (by team ID, as on `main`), and sends **no `X-Mirador-Project` header** (already the contract on `main`: `internal/relay/send.go:44`, `internal/delivery/delivery.go:166`, and `internal/cli/relay_deps.go:82` omit `X-Mirador-Project` when sending with a server key). "terma teams" refuses server-key profiles.

### Account- and generation-checked writes to `routing.State` (CAS)

Every profile keeps one account-stamped state file (`stateDir/routing/<profile>.json`) with `State.Account` (a valid `auth.AccountKey` or `sendas.NoPersonLane`, enforced by `State.check()`).

`internal/routing/store.go` enforces compare-and-set on every mutation under `flock.Locked`:
- `Create(stateDir, profile, account, change)` — stamps `s.Account = account`, refuses if a state file already exists.
- `Update(stateDir, profile, expect, change)` — takes `Expect{Account string, Generation *int64}`; refuses with `ErrAccountMismatch` if `s.Account != expect.Account`, or `ErrStaleGeneration` if `expect.Generation != nil && s.Generation != *expect.Generation`.
- `Replace(stateDir, profile, expectPrev, newAccount, change)` — atomically replaces an existing state (used by `terma setup` on account switch `alice` → `bob`, or server-key setup), verifying the live profile account matches `newAccount` before committing.
- `Delete(stateDir, profile, expect)` — removes the state file only if the on-disk state still matches `expect`.

**Consequences:**
- An open interactive "terma teams" session captures `(Account, Generation)` on load and passes `Expect{Account, &gen}` on write. On `ErrAccountMismatch` or `ErrStaleGeneration`, it fails cleanly and **never** replays stale user consent or bindings into Bob's or a concurrently modified state.
- The policy refresher checks `State.Account == Profile.Account` (via `readLaneAsOne`) before fetching and passes `Expect{Account: profileAccount}` into `routing.Update`, skipping and failing closed on mismatch.

### Listing, ticking, unticking, and refreshing teams

1. **Listing ("terma teams"):** One shared `api.Client` (serializing token refreshes via `tokenGen` and the cross-process credential lock) lists all organizations and concurrently (`listLimit = 4`) fetches each org's teams and policies. Every org is visible immediately—no `SignedIn: false` rows and no `--authorize`. `403 PROJECT_NOT_FOUND` hides a deleted team; `403 ORG_NOT_ACCESSIBLE` annotates the org as inaccessible without failing other orgs.
2. **Ticking a team:** Fetches its policy (`GET /v1/policy?project_id=<team_id>`), stamps `(TeamID, OrganizationID, AuthURL)`, restricts `ModeGlobal` to `o.Type == "personal"`, runs `decideBindings` (unchanged local consent model), and commits via CAS `routing.Update` (`State.Select` + binding ops). No key minting; the team is immediately sendable.
3. **Unticking / releasing a team:** `State.Deselect(enrollmentID)` removes the enrollment via CAS `routing.Update`. Bindings stay so bound repos never spill into Personal.
4. **Per-team policy refresh (`teamrefresh.go`):** Refreshes each enrollment every 1 minute (`PolicyRefreshInterval`, or 24 hours `RefusedRecheck` while `Refused`) and commits strictly through CAS `routing.Update(ApplyRefresh)` / `routing.Update(ApplyOrgAccessLost)`. Enrollments from another `AuthURL` fail closed (`ErrOtherEnvironment` → `Transient`).

---

## 3. Interaction with #152's Lanes, Per-Session Owner, Pinned Destinations, and Version Skew

Lanes and multi-org routing are orthogonal axes:
- **Lane (`account:<16-hex-key>` or `no_person`) = WHO collected the record.** Fixed at write/arrival time; immutable; governs which credential sends the record and what `terma.user.id` may be stamped.
- **Destination (`Profile` + `EnrollmentID` + `Location` + `HoldKey` + `Run`) = WHICH team the record was routed to.** **Pinned once routed** (including for held records); later checks only wait, send to that enrollment, or discard—never re-route.
- **Session owner (`<id>.owner` + `Manifest.Account`) = WHO owns a session's local attribution manifest.** Atomically claimed on first write; cross-account writes fail closed (`ErrAccountConflict` → `account_conflict`).

### Subsystem-by-subsystem invariants

1. **Hooks (`internal/hooks/hookrun`, `internal/cli/hook.go`, `internal/spool`):**
   - `hookProfile` reads `config.json`, `user-credentials.json`, and `routing.State` in one consistent snapshot (`readLaneAsOne`): `ServerKeySignIn` → `no_person`, else `auth.Account(...)` checked against BOTH `Profile.Account` AND `routing.State.Account`. Once routing is active, a missing, corrupt, or wrong-account `routing.State` **fails closed** (never falls back to `config.Profile.Team`).
   - `Spool.Bind` and `session.Store.Bind` bind the lane; signed-out or unreadable drops before any write and counts in `spool/losses/`.
   - `claim.Claim` (with `CatchAll bool` renamed off `Profile`) and `claim.Placement` carry both `Account` (lane) and `Profile`/`Kind`/`Run` (routing), with empty `ProjectID`.
   - `spool.Event` carries both `Account`/`NoPerson` and `Profile`/`Enrollment`/`HoldKey`/`Run`/`OwnLocation` in `events-v2.jsonl`.
   - **Sandboxed git hooks:** `Routing.GitHooks` decides hook installation and commit stamping. If a sandboxed git hook cannot read `routing.State`, it parks its record with **no destination** (`Enrollment == ""`); when an unsandboxed hook unparks it (or delivery reads it), it is **dropped and counted** (`no_destination` in `spool/losses/`), never filled in later or sent to a default team.
   - **Extension agents (OMP, Pi, OpenCode, DSH, Hermes):** All 5 `hookrun.Extension` agents call `StartRun` on non-subagent session start AND emit `<prefix>-tool-reach` for read/search/shell tool calls **before** routing activates (PR 9).

2. **Relay ingress, interval Mixed, pinned holds, and `.teams/` outbox (`internal/relay`, `internal/relay/daemon`):**
   - **Arrival lane (`arrivalLane`):** Resolved BEFORE reading the body (3 s versioned `laneState` retry); signed-out/unreadable drops immediately into `losses.json` and never enters hold or outbox.
   - **Interval Mixed coverage (`pieces.go`):** Every OTLP record carries an interval `[start, end]` (spans `[StartTimeUnixNano, EndTimeUnixNano]`, logs `[min, max]` of `TimeUnixNano`/`ObservedTimeUnixNano`, metrics `[StartTimeUnixNano, TimeUnixNano]` across Sum/Gauge/Histogram/ExponentialHistogram/Summary points). `relay.Record` and `relay.Queued` store both `At` and `EndAt`. A record is Mixed if **`!endAt.Before(mixedAt)`**—so a span or interval metric starting before `MixedAt` and ending at or after `MixedAt` counts as Mixed and is dropped.
   - **Two separate memory holds (`hold.go`):**
     - `unknownHold`: unplaced records waiting for a hook placement (carries arrival `Narrow` flag and `account`). Only these call `Router.Route` once placed.
     - `pinnedHold`: once `Router.Route` assigns an `EnrollmentID` with `Outcome == Hold` (`ReasonPending` or `ReasonBlocked`), the record's destination (`Profile`, `EnrollmentID`, `Location`, `HoldKey`, `Account`) is **pinned immediately**. Later sweeps call only `Recheck(pinnedDest, now)` → `Wait` / `SendNow` (into that enrollment's outbox) / `Discard`—**never** re-routing to Personal or another team. Unfiltered parts stay in memory (`policyHold` 15 m / `policy_timeout`) and never hit disk before content policy is applied.
   - **Separate outbox layout (`teamoutbox.go`):** Routed parts live strictly under **`outbox-v2/.teams/@<account>/<enrollment>/<tool>/`** (`teamRoute{account, enrollment, tool}`), cleanly isolated from the pre-cutover project-keyed layout (`outbox-v2/@<account>/<project>/<tool>/<repo>/`). After cutover (PR 10), relay startup deletes any leftover parts outside `.teams/` and counts every dropped record in `losses.json` (`no_destination`), never stranding them silently.
   - **Fenced send admission (`teamsender.go`):**
     1. Check `mayLeave(rt.account, serverKey)` first—another person's lane waits without touching their routing state or token, aging out at 14 days (`outbox_expired`).
     2. Load batch, run `Rechecker` + content filter, obtain bearer token (`SignIn.Token(ctx, rt.account)` or `keys.json` for `no_person`), stamp `terma.user.id` for `rt.account` alone.
     3. **Pre-POST admission fence (no lock held across HTTP):** Re-verify `(routing.State.Generation, runs.Snapshot, mayLeave)` immediately before `HTTP.Do(req)`. If the enrollment ended, became `Refused`, no longer covers the location, the run turned Mixed covering `[At, EndAt]`, or the lane switched between load and send, abort and discard (or wait on lane switch) without posting.
     4. **Cross-account sweep guard:** `dropDeselected` only inspects senders whose `rt.account == activeAccount` (`mayLeave`); Bob replacing `routing/default.json` never discards Alice's `.teams/@<alice>/` backlog as `why_deselected`.
   - **Queue-aware ledger pruning (`runs/prune.go` + `daemon/ledger.go`):** `runs.Prune` retains every session present in `relay.QueuedSessions()` (`.teams/` outbox) or `spool.Sessions()` (`events-v2.jsonl`).

3. **Spool delivery (`delivery/routed.go`):**
   - Checks lane (`Account`/`NoPerson`) FIRST—another lane's events wait untouched (age out at 14 days).
   - Events with `Enrollment == ""` (queued before activation or parked without routing state) are **dropped and counted** (`no_destination`), never sent to `Profile.Team` or re-routed.
   - Verifies `routing.State.Account` matches the event's lane, runs `Recheck` + `HoldKey` + run-ledger Mixed check, and sends per enrollment with `SignIn.Token` + `X-Mirador-Project` (or `keys.json` with no `X-Mirador-Project` for `no_person`).
   - **`runs.Locational` post-Mixed contract (decided: KEEP, §6 item 9):** Non-content events that route by their own checkout location (`OwnLocation: true` && `runs.Locational(name)`—commits, pushes, file-touch metadata) still leave to their own repository's team after a session's run turns Mixed, whereas all conversation/prompt/reply/title/OTLP telemetry drops from `MixedAt` onward.

4. **Relay version-skew protection:**
   - The new relay records `Routing: true` in `daemon.RunInfo` (`relay/run.json`).
   - **At activation (`terma setup`, `terma update --refresh`):** supersedes any running pre-routing relay and verifies no relay is running with `!RunInfo.Routing` before writing/activating `routing.State`.
   - **Defense in depth at hook time:** if `RunningRelay` reports a live relay with `!info.Routing`, the hook triggers `daemon.Supersede` immediately and does **not** write an empty-`ProjectID` placement (preventing an older relay from reading it as `Mark` / `not_collected`).

---

## 4. `ORG_NOT_ACCESSIBLE` Per Request, Ordered Refresh, and Relay Behavior

### Server contract
With a single user token, user-token refresh (`POST /v1/auth/user/token`) is account-wide and never returns `ORG_NOT_ACCESSIBLE`. Instead:
- **`GET /v1/policy?project_id=<team_id>`** returns **HTTP 403 `PERMISSION_DENIED`** with AIP-193 reason:
  - `ORG_NOT_ACCESSIBLE` — authoritative loss of membership/access to that organization;
  - `PROJECT_NOT_FOUND` — team deleted or not in the org;
  - `AUTHORIZATION_UNAVAILABLE` — transient authorization service failure.
- **OTLP ingest (`POST /v1/{traces,logs,metrics}` with `X-Mirador-Project: <team_id>`)** returns **HTTP 403** with `google.rpc.ErrorInfo.reason == "ORG_NOT_ACCESSIBLE"` (parsed by `sendas.Reason` from protobuf or JSON).

### Ordered refresh fencing (`Epoch` on every state change)
- `State.ApplyRefresh` advances `Team.Epoch++` on **every** state-changing outcome: **policy success (`r.Policy != nil`)**, `Terminal`, `OrgAccessLost`, and `SignIn`.
- A result whose `r.Epoch != t.Epoch` is discarded, so a delayed refusal or delayed transient error started before a newer success can never overwrite the newer success (and vice versa).
- For org-wide `ORG_NOT_ACCESSIBLE`, the refresher snapshots `(enrollmentID -> epoch)` across all enrollments in `t.OrganizationID` **before** issuing the HTTP request, and applies `ApplyOrgAccessLost(orgID, snapshotEpochs, at)` inside CAS `routing.Update(Expect{Account})`: every enrollment of **that account** in `orgID` whose `Epoch` still matches its pre-request snapshot transitions immediately to `Refused` (`Refusals = 2`, `Epoch++`, `RefusedAt = at`, `LastError = OrgAccessLost`), while any sibling enrollment that already advanced via a newer successful refresh is spared. Bindings and blocking policies remain in place; other organizations and Personal continue uninterrupted.

### Classification and behavior at every surface

1. **Policy refresh (`policy/team.go` & `daemon/teamrefresh.go`):**
   - `403 ORG_NOT_ACCESSIBLE` → `routing.OrgAccessLost` → org-wide `ApplyOrgAccessLost` (immediate `Refused`, 24 h `RefusedRecheck`, no keystore deletion).
   - `403 PROJECT_NOT_FOUND` → `routing.Terminal` (1st: `Suspended`, 2nd consecutive: `Refused` + 24 h `RefusedRecheck`).
   - `403 AUTHORIZATION_UNAVAILABLE` / bare `403` / `5xx` / `429` / network → `routing.Transient` (last policy stays in force until it expires; `LastError = Transient` written only if `r.Epoch == t.Epoch`).
   - `401` / `auth.ErrNotLoggedIn` / `auth.ErrSignInRetired` / `auth.ErrWrongEnvironment` → `routing.SignIn` (`SignInNeeded` per team so matched org repos still block and never spill into Personal; `terma setup` re-signs in once and refreshes all enrollments).
   - **State-change wakeup & backoff reset:** Whenever a refresh transitions an enrollment (either recovering to `Active` or becoming `Refused`), the refresher notifies the relay engine to wake that enrollment's `teamSender` **immediately** (ignoring any active `refusedBackoff` timer) and **resets `refusedBackoff` to 0** on recovery—so recovered teams resume sending at once and `Refused` queues purge immediately via `Recheck → Discard`.
2. **OTLP ingest in the relay (`internal/relay/send.go` & `teamsender.go`):**
   - On `HTTP 403` with `sendas.Reason(answer) == "ORG_NOT_ACCESSIBLE"` (user token, `pol.Key == ""`):
     - **Never** calls `SignIn.Refused` or `SignIn.Retired` (other orgs & Personal keep sending).
     - Backs off only that enrollment's sender (`refusedBackoff`, 5 min → 1 hour).
     - Triggers a **coalesced, non-blocking refresh nudge** (`refresher.Nudge(enrollmentID)`) so the policy endpoint confirms `ORG_NOT_ACCESSIBLE` right away through the same ordered `Epoch` + CAS fence. Once `ApplyOrgAccessLost` commits `Refused`, the state-change wakeup interrupts the backoff timer and `Recheck → Discard` purges the dead queue immediately.
3. **Spool delivery & heartbeats:**
   - `delivery/routed.go`: `403 ORG_NOT_ACCESSIBLE` backs off only `retryDest(enrollment)` without calling `SignIn.Refused`/`Retired`; the next flush after `Refused` discards queued events via `Recheck`.
   - `relay_teams.go` heartbeat: `403 ORG_NOT_ACCESSIBLE` does not rotate/retire the user token and falls through to the next `Active` team (personal first, then org teams in order).

---

## 5. Re-Cut PR Stack (Ordered Around Activation; All PRs < 1,400 Prod Lines)

**Order principle:** Consumers & core data structures land first (inactive until a `routing.State` exists) → crossing detection & all producers (including all 5 `Extension` agents) land before activation → **PR 10 is the single atomic activation/cutover PR** (`setup`, `--desktop-grant`, server-key `routing.State`, `teardown`, `switch` removal, version-skew gate, old-layout/no-destination cleanup, and removal of all single-team fallbacks) → "terma teams" and diagnostics land cleanly on top of the active routing state. Every PR compiles, passes `make check`, leaves `main` safe to ship, and ships its own invariant tests.

| # | PR title, scope & invariant tests | Prod lines (est.) | Kept / Rewritten | Test lines (est.) |
|---|---|---|---|---|
| **1** | **Routing CAS, ordered refresh & per-team policy refresh**<br>Scope: `internal/routing/{state,store,recheck}.go` (`State.Account`, CAS `Expect{Account, Generation}` on `Update`/`Create`/`Replace`/`Delete`, `Epoch++` on every state-changing refresh including success, org-wide `ApplyOrgAccessLost`, `RefusedAt`, `Team`, `SameGeneration`, `NewEnrollmentID`), `api.ReasonProjectNotFound`, `policy/team.go` (`FetchTeam`/`RefreshTeam`/`Classify` on user token + `keys.json`).<br>*Invariant tests:* CAS rejects wrong account & stale generation (Alice vs Bob); delayed refusal/transient cannot undo a newer success; `ApplyOrgAccessLost` refuses all matching-epoch enrollments in org while sparing newer-epoch siblings and other orgs/Personal; `Classify` table for 403 `ORG_NOT_ACCESSIBLE`, 403 `PROJECT_NOT_FOUND`, 403 `AUTHORIZATION_UNAVAILABLE`, 401/retired. | ~320 | ~120 / ~200 | ~600 |
| **2** | **Run ledger, `Claim.CatchAll` rename + location-only claims & spool destinations**<br>Scope: `runs/` (as-is, queue-aware `prune.go`), `boundary/runs_test.go`, `internal/relay/claim/` (rename `Claim.Profile bool` → `Claim.CatchAll bool` first, then `at.go`, `Kind`, `Run`, `Profile string` preserving `Account`), `internal/spool/spool.go` (`Profile`/`Enrollment`/`HoldKey`/`Run`/`OwnLocation`/`Sessions()` preserving v2 lanes).<br>*Invariant tests:* Concurrent `runs.Touch`, >64 location bound → Mixed, sidecar persistence, queue-aware `Prune`, `Locational` pin; `CatchAll` vs `Profile` separation & `Claim.At` preserving `Account`; spool destination round-trip & untagged-drop accounting. | ~1,160 | ~1,080 / ~80 | ~1,500 |
| **3** | **Shell reach core: AST walk, nesting bounds, word expansion & substitution**<br>Scope: `internal/hooks/hookrun/{shellnest,shellwalk,shellexpand,shellsubst,firstwords}.go` + advisor fixes (`{ ... } \| bash`, `bash -o pipefail`, `bash < file`, zsh remainder, non-blocking FIFO).<br>*Invariant tests:* Nesting/expansion bounds fail closed to `UnknownReach`; subshell/block pipes & `eval`/`trap`/`<shell> -c` parsed; secret masking in `firstwords`; FIFO does not block past 20 ms deadline. | ~1,330 | ~1,250 / ~80 | ~1,000 |
| **4** | **Shell reach resolution: path lookup, folder-below, command rules & `Classify`**<br>Scope: `internal/hooks/hookrun/{shellcommands,shellpaths,shelllookup,shellbelow,reach}.go` + advisor top fixes (count every cached folder sighting as operand, ignore `find`/`fd` depth with `-exec`/`-x`, don't stop folder-below at enclosing checkout or `$HOME`, Glob `~`, URL/UNC share safety).<br>*Invariant tests:* Composition fuzz property (`Y; X` reaches everything `X` reaches; `ls .. && rg -n TODO ..` caught); `find`/`fd -maxdepth 1 -exec`/`-x` caught; enclosing repo/`~/.git` & Glob `~` caught; 20 ms deadline test (deep trees, FIFOs); routine command corpus stays quiet. | ~1,320 | ~1,150 / ~170 | ~1,200 |
| **5** | **Relay engine: interval Mixed splitting, pinned holds, `.teams/` lane outbox & fenced `teamSender`**<br>Scope: `internal/relay/{teams,pieces,hold,teamoutbox,teamsender,forward,route,process,stats,send}.go` — interval `[start, end]` timestamps for spans/logs/metrics, separate `unknownHold` vs destination-pinned `pinnedHold` (`Recheck` only, never re-routes), distinct `outbox-v2/.teams/@<account>/<enrollment>/<tool>/` layout + old-layout purge/loss counter, fenced pre-POST admission check (no lock across HTTP), `dropDeselected` scoped to active account's lane, ingest `403 ORG_NOT_ACCESSIBLE` coalesced refresh nudge + backoff reset & immediate wake on state change.<br>*Invariant tests:* Held `Pending`/`Blocked` parts never re-route on state change; spans & interval metrics straddling `MixedAt` drop; pre-POST admission fence aborts on concurrent Mixed/deselect without holding lock over HTTP; `dropDeselected` spares Alice's `.teams/@<alice>/` backlog under Bob; ingest 403 never calls `SignIn.Refused`/`Retired` and triggers refresh nudge + backoff reset; `.teams/` isolation & old-layout purge counted in `losses.json`. | ~1,260 | ~760 / ~500 | ~1,300 |
| **6** | **Relay daemon: `daemon.Teams`, ordered CAS refresher, queue-aware pruner, `RunInfo.Routing` & heartbeat**<br>Scope: `internal/relay/daemon/{teams,teamprofiles,teamrefresh,ledger,engine,run,stop,refresh}.go` (`RunInfo.Routing = true`, `State.Account` check, CAS `Update(ApplyRefresh/ApplyOrgAccessLost)`, coalesced `Nudge`, `OnStateChange` wakeup, queue-aware `runs.Prune`, server-key `keys.json` validation with no `X-Mirador-Project`), `internal/cli/{relay_teams,relay_deps,relay_run}.go`.<br>*Invariant tests:* Refresher skips & fails closed on `State.Account != Profile.Account`; org-wide `ORG_NOT_ACCESSIBLE` refuses matching-epoch enrollments via CAS; ingest 403 nudge refreshes immediately and wakes sender past backoff; `runs.Prune` preserves sessions in `.teams/` outbox and `events-v2.jsonl`; `RunInfo.Routing` written & readable; heartbeat skips `403 ORG_NOT_ACCESSIBLE` to next Active team. | ~980 | ~580 / ~400 | ~1,100 |
| **7** | **Routed spool delivery**<br>Scope: `internal/delivery/{routed,delivery,content}.go`, `internal/cli/spool.go` (lane gate first, `State.Account` pairing, `Recheck` + `HoldKey` + `runs.Locational` Mixed check, destination-less events dropped & counted once active, `SignIn.Token` + `X-Mirador-Project`, server-key `keys.json` with no `X-Mirador-Project`, per-enrollment 403 backoff).<br>*Invariant tests:* Wrong-lane events held untouched before routing/policy reads; destination-less events dropped & counted (never sent to `Profile.Team`); `runs.Locational` events leave post-Mixed while content/conversation events drop; server-key delivery sends no `X-Mirador-Project`; ingest 403 backs off only failing enrollment without retiring token. | ~460 | ~240 / ~220 | ~1,100 |
| **8** | **Routed hooks, 6 core agents' reach wiring, `StartRun` & git hooks via `Route`**<br>Scope: `internal/hooks/hookrun/{routing,placement,turns,hookrun,lifecycle,commit,push,relay,lanes,evidence,funding,observation,title}.go`, `dispatch/`, `hookmgr/` (Claude upgrade refresh), `repohooks/`, `agents/internal/reach/`, 6 agents (`claude`, `codex`, `copilot`, `gemini`, `cursor`, `antigravity`), `cli/hook.go` (snapshot with `routing.State.Account` + `RunInfo.Routing` skew guard), `scripts/bench-hook.sh` + `scripts/benchhooks/main.go`.<br>*Invariant tests:* Cross-hook & cross-tool Mixed across all 6 agents + Codex per-turn routing; `hookProfile` fails closed on `State.Account != lane`; `<id>.owner` `account_conflict` preserved; sandboxed hook without `routing.State` parks empty destination and `unpark` drops & counts `no_destination`; pre-routing relay (`!RunInfo.Routing`) superseded with no empty-`ProjectID` claim written; latency budget test for Read/Bash/commit hooks (<50 ms). | ~1,340 | ~1,000 / ~340 | ~2,200 |
| **9** | **Extension agents' `StartRun` & read/shell crossing hooks (OMP, Pi, OpenCode, DSH, Hermes)**<br>Scope: `internal/hooks/hookrun/extension.go` (`StartRun` on non-subagent `sessionStart`, `<prefix>-tool-reach` handler), plugin templates (`omp/terma-omp.ts`, `pi/terma.ts`, `opencode/plugin/terma.js`, `dsh/plugin/terma.mjs`, `hermes/plugin/__init__.py`) + extension refresh trigger.<br>*Invariant tests:* Per-agent tests confirming `session-start` starts a run (subagent start does not) and `tool-reach` marks a run Mixed on cross-org Read/Grep/Glob/Bash calls across all 5 Extension agents; plugin template contract tests. | ~420 | ~0 / ~420 | ~600 |
| **10** | **Activation & cutover: unified `terma setup`, `--desktop-grant`, server-key `routing.State`, `terma teardown`, remove `terma switch` & single-team fallbacks**<br>Scope: `internal/cli/{setup,setup_agents,setup_teams,setup_grant,setup_serverkey,teardown}.go`, `internal/setup/setup.go`, delete `internal/cli/switch.go`, relay version-skew activation gate (`RunInfo.Routing`), account-switch CAS `Replace`, old-layout outbox & destination-less spool cleanup, delete single-team fallbacks in hooks/relay/delivery.<br>*Invariant tests:* Missing/corrupt/wrong-account `routing.State` fails closed across hooks, relay, and delivery (zero fallback to `Profile.Team`); `--desktop-grant` ticks only `--team`, accepts binding defaults under `-y`, never enables Personal; server-key setup writes 1-team `routing.State` (`Account: "no_person"`) validating team/authURL; activation gates on `RunInfo.Routing` relay; account switch CAS rejects stale Alice writer; pre-activation destination-less spool & old-layout outbox parts dropped & counted; sign-in recovery refreshes all enrollments. | ~650 (−~500 del) | ~220 / ~430 | ~900 |
| **11** | **Multi-team "terma teams" command**<br>Scope: `internal/cli/{teams,teams_list,teams_tick,teams_bind,teams_change,teams_checklist,teams_names,root}.go` + interactive checklist hookup in `setup_teams.go`. Single user token lists all orgs/teams concurrently; CAS `Expect{Account, &gen}` on every write; refuses server-key profiles; no `--authorize`, no `--org` re-auth, no key minting.<br>*Invariant tests:* Stale-generation & cross-account CAS conflict rejects Alice's open "terma teams" without replaying consent; server-key profile refused; `403 PROJECT_NOT_FOUND` hides deleted team; `403 ORG_NOT_ACCESSIBLE` annotates org without failing others; deselect keeps bindings. | ~1,280 | ~900 / ~380 | ~900 |
| **12** | **Diagnostics, E2E & docs: "terma route explain", `terma doctor`, scratch-machine E2E & docs**<br>Scope: `internal/cli/{route,doctor_routing,doctor_probes}.go`, `internal/doctor/{teams,run,status,verdict}.go` (merged with #152 `losses.json` + `spool/losses/`, single `terma setup` remediation for `SignInNeeded`), `multiorg_e2e_unix_test.go`, `AGENTS.md`, `README.md`.<br>*Invariant tests:* `doctor` fails on unreadable/missing/wrong-account `routing.State`, suspended/refused teams, pending bindings; shows `terma setup` fix for `SignInNeeded`; combines Mixed runs with #152 losses; `route explain` matches `Router.Route` across all cases; full scratch-machine E2E on user token (multi-org routing, Mixed cutoff, org-wide `ORG_NOT_ACCESSIBLE`, account switch). | ~780 | ~550 / ~230 | ~800 |
| **Total** | **12 PRs (max ~1,340 prod lines/PR; all < 1,400)** | **~11,300** | **~7,850 / ~3,450** | **~13,200** |

---

## 6. Decided Design Questions

All 9 design questions (8 from Phase 1 + Council 12c) are resolved:

1. **Org-wide `ORG_NOT_ACCESSIBLE` on policy fetch:** Refuses **every enrollment of that account in that organization** (`ApplyOrgAccessLost`), fenced by pre-request per-enrollment `Epoch` snapshots so a delayed result never undoes restored access on a sibling enrollment. Bindings and blocking policies remain in force; other organizations and Personal continue uninterrupted.
2. **Relay ingest `403 ORG_NOT_ACCESSIBLE`:** Triggers a **coalesced, non-blocking refresh nudge** for that enrollment, applied through the same ordered `Epoch` + CAS fence. State-change rechecks, purges, and backoff resets wake the sender immediately without waiting on `refusedBackoff`.
3. **`terma setup --desktop-grant -y --org <orgID> --team <teamID>`:** Ticks **only `--team`** (preserving any already-ticked teams for the same account); `-y` accepts that team's binding defaults and **never** enables Personal.
4. **Server-key (`TERMA_API_KEY`) profile representation:** A one-team `routing.State` with `Account: "no_person"`. Setup validates the key's team and environment (`ServerKeyAuthURL`), keeps the key in `keys.json`, and sends **no `X-Mirador-Project` header** (verified on `main`: `internal/relay/send.go:44`, `internal/delivery/delivery.go:166`, and `internal/cli/relay_deps.go:82` already omit `X-Mirador-Project` for server keys).
5. **Cross-user sign-in on the same profile (`alice` → `bob`):** One account-stamped `routing.State` per profile (`routing/<profile>.json`). Setup replaces it conditionally (`routing.Replace` CAS) for the new account and rejects outstanding writers of the old account (`ErrAccountMismatch`).
6. **OMP and other `hookrun.Extension` agents (Pi, OpenCode, DSH, Hermes):** All 5 receive `StartRun` AND read/shell crossing hooks (`<prefix>-tool-reach`) **before** multi-org routing activates, in their own focused PR (PR 9) to stay under the 1,400-line cap.
7. **PR #140 advisor shell-reach findings (`ae594662`, rated 6/10):** All top advisor fixes—including the composition fuzz property and bounded, non-blocking filesystem/FIFO behavior—are folded directly into PRs 3–4.
8. **`SignInNeeded` on one user token:** Keep per-team `routing.SignInNeeded` (so matched org repos continue to block and never spill into Personal), gate sending on sign-in account-wide, remediate in `terma doctor` with a single `terma setup`, and refresh all enrollments after sign-in completes.

9. **`runs.Locational` post-Mixed exception (decided: KEEP):** Commits, pushes, and file-touch metadata that carry zero prompt/conversation/tool content and route by their own checkout location (`OwnLocation: true` && `runs.Locational(name)`) still leave to their own repository's team after a session's run turns Mixed. All conversation, prompt, reply, title, and OTLP telemetry content is dropped from `MixedAt` onward.

---

## 7. Council Changes

Every item from `council-mir708-decisions.md` is incorporated below; no item was pushed back on.

| Council item | Where handled in this plan |
|---|---|
| **1. Account- and generation-checked CAS writes to `routing.State`** | §2 (*Account- and generation-checked writes*), §5 **PR 1** (`Expect{Account, Generation}` on `Update`/`Create`/`Replace`/`Delete`), **PR 6** (refresher skips & fails closed when `State.Account != Profile.Account`), **PR 10** (account-switch CAS `Replace` & teardown CAS `Delete`), **PR 11** ("terma teams" captures `(Account, Generation)` and never replays stale consent on conflict). |
| **2. Refresh results apply in order** | §4 (*Ordered refresh fencing*), §5 **PR 1** (`ApplyRefresh` bumps `Epoch++` on success as well as failures; `ApplyOrgAccessLost` checks pre-request per-enrollment epoch snapshot), **PR 6** (ordered refresh + coalesced nudge through the same fence). |
| **3. Record destination pinned once routed (including held records)** | §3 (*Two separate memory holds*), §5 **PR 5** (`unknownHold` vs destination-pinned `pinnedHold` that only runs `Recheck` and never re-routes, fixing #140's `hold.go`). |
| **4. Separate outbox layout for routed parts + old-layout loss accounting** | §3 (*Separate outbox layout*), §5 **PR 5** (`outbox-v2/.teams/@<account>/<enrollment>/<tool>/` + old-layout purge helper), **PR 10** (relay startup deletes & counts leftover pre-cutover parts in `losses.json`). |
| **5. Re-cut stack around activation; every PR < 1,400 prod lines** | §5 (all 12 PRs strictly ≤ 1,340 prod lines; consumers → crossing/producers incl. Extension agents → **PR 10** unified setup/teardown/switch removal & fail-closed cutover → **PR 11** "terma teams" → **PR 12** diagnostics/E2E). |
| **6. Records with no destination at activation dropped & counted** | §3, §5 **PR 7** (spool delivery drops & counts `Enrollment == ""` as `no_destination`, never falls back to `Profile.Team`), **PR 10** (old-layout outbox & pre-activation spool cleanup). |
| **7. Version skew guard against pre-routing relay** | §3 (*Relay version-skew protection*), §5 **PR 6** (`RunInfo.Routing = true`), **PR 8** (hook defense-in-depth supersedes `!RunInfo.Routing` relay and withholds empty-`ProjectID` claims), **PR 10** (activation verifies no pre-routing relay is running before writing `routing.State`). |
| **8. Invariant tests ship with each PR** | §5 (every PR 1–12 lists its required invariant tests inline). |
| **9. Mixed covers intervals (`[start, end]`)** | §3 (*Interval Mixed coverage*), §5 **PR 5** (`pieces.go`, `Record`/`Queued` `At`+`EndAt`, `!endAt.Before(mixedAt)` for spans, logs, and interval metrics). |
| **10. Send admission is fenced + cross-account `dropDeselected` guard** | §3 (*Fenced send admission*), §5 **PR 5** (pre-POST versioned re-check with no lock across HTTP; `dropDeselected` restricted to active account's lane so Bob never purges Alice's `.teams/@<alice>/` backlog). |
| **11. `Claim.Profile` bool collision** | §1, §3, §5 **PR 2** (rename `Claim.Profile bool` → `Claim.CatchAll bool` before adding `Claim.Profile string`). |
| **12a. `refusedBackoff` resets on successful refresh** | §4, §5 **PR 5 & PR 6** (`OnStateChange` wakes `teamSender` past backoff and resets `refusedBackoff` to 0 on recovery). |
| **12b. Refresher commits through `routing.Update(ApplyRefresh)`** | §2, §4, §5 **PR 6** (refresher always commits via CAS `routing.Update`). |
| **12c. `runs.Locational` records leave after Mixed + human sign-off** | §3 (*Spool delivery*), §6 (*Remaining human sign-off item*), §5 **PR 7** invariant test. |
| **12d. Keep `runs/prune.go` queue-aware** | §1, §3, §5 **PR 2 & PR 6** (`daemon/ledger.go` feeds `.teams/` outbox + `spool.Sessions()` into `runs.Prune`). |
| **12e. Sandboxed hook without `routing.State` drops & counts on unpark** | §3 (*Hooks*), §5 **PR 8** (parks with `Enrollment == ""`; `unpark` drops & counts `no_destination`). |
| **12f. Latency budget test for reach hooks** | §5 **PR 4** (20 ms shell scan deadline test) & **PR 8** (Read/Bash/commit hook <50 ms latency test & `bench-hook.sh`). |
| **12g. Server contract stated (`GET /v1/policy` → 403 `ORG_NOT_ACCESSIBLE`)** | §4 (*Server contract*). |
| **Decided Q1–Q8** | §2, §3, §4, §5 (**PR 1, 3, 4, 5, 6, 7, 9, 10, 11, 12**), and §6. |
