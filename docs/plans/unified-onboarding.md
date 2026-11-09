# Unified desktop onboarding: terma-cli

The desktop apps install terma-cli in the background and sign it in without a second browser approval. This plan is
the CLI's part. The overall flow, the token design and the organization model are in
`terma-desktop/docs/plans/unified-onboarding.md`; the endpoints are in
`mirador-platform/docs/plans/unified-onboarding.md`.

Status: proposal, revised 2026-11-09, including Dawson's answer that the CLI "stays as personal, but has the option
to switch to a different org". The delegated grant is **Council: approved 2026-11-09 with the changes listed**
(refresh-authenticated on the desktop's side; reuse only on a matching user and organization). The organization-model
council (same date, approve with changes) added one CLI item: the cutover when the organization changes (1a), proved
on the existing routing by a transition test.

## Goal

`terma setup` can be driven by a desktop app end to end with no terminal and no browser: the agents, the
organization and the team come as flags, and the sign-in comes as a delegated authorization code the desktop obtains
and writes to the CLI's stdin. Onboarding passes the desktop's PERSONAL organization and its one team; later the
desktop runs the same command to switch the CLI to another organization and team the person picks. The CLI keeps
making its own PKCE pair, exchanging the code itself and storing its own credential, so its session is its own and
nothing the desktop holds can replay it.

## What exists

- `internal/cli/setup.go`: `newSetupCommand` with `--harness`, `--org`, `-y`, `--no-browser`, `--relay-service`,
  `--relay-addr`, `--insecure-storage`, `--managed-config`; the team comes from the root's persistent `--team`/`-t`
  (`internal/cli/root.go`). `runSetup` builds `setup.Steps`; the `SignIn` step calls `app.setupSignIn`
  (`internal/cli/signin.go`).
- `internal/cli/signin.go` `signIn`: refuses with `TERMA_API_KEY` set; `reuseStoredSession` loads the stored
  credential for the wanted organization id (`auth.LoadCredentialFor`), verifies it with `/v1/whoami`
  (`verifyCredential`) and reuses it. It compares the organization only; the user is not checked. Else
  `auth.Login(ctx, client, auth.LoginOptions{...})` opens the browser (`internal/account/auth/login.go`), then
  `auth.SaveCredential` (which returns the replaced credential, then revoked) and `config.UpdateProfile(...
  applyLogin)`. A browser approval for another organization than `--org` is only noted.
- `internal/account/auth/store.go` `Credential` carries `OrganizationID` and `UserID`. A profile keeps one credential
  per organization (`profileCredentials.Organizations`); `saveReplacing` replaces, and `signIn` revokes, only the same
  organization's previous one.
- `internal/cli/switch.go` `terma switch`: revokes every organization's sign-in, forgets the organization and team,
  and reruns setup, which asks for them (browser, then pickers). The desktop runs it today from the Agents page.
- `internal/account/auth/login.go` `Login`: PKCE (`pkce.go` `NewPKCE`), a loopback listener, `buildAuthorizeURL`,
  waits for the callback, calls `exchanger.ExchangeCode(ctx, code, verifier, port)` on `TokenExchanger`.
  `DefaultLabel` is there too.
- `internal/account/api/token.go` `ExchangeCode`: `POST /v1/auth/cli/token` with `grant_type: authorization_code`,
  `code`, `code_verifier`, `redirect_port`. Its tests are `token_unix_test.go` and `client_test.go`.
- `internal/cli/project.go` `selectPolicyTeam`: `--team` matches, else the only team (`soleOrPick`), else a picker
  when it can ask; `canPrompt()` (`confirm.go`) is false without a terminal, so `-y` from a desktop never blocks.
  `askOrganization` (`signin.go`) asks only when it can.
- A team with no policy collects nothing (`config.NoPolicy`, `internal/doctor/checks.go` `NoPolicyStep`).
- `internal/cli/auth.go`: the hidden `terma auth token --json` for the terma-mcp bridge. Its allowlisted output and
  its error codes are the model for anything printed to another program.
- install.sh: `~/.local/bin`, no sudo, PATH line in the shell startup files. Windows config and state directories
  (`%APPDATA%`, `%LOCALAPPDATA%`) are resolved in `internal/config/config.go`.
- The CLI's own browser sign-in keeps `/cli/auth` (`buildAuthorizeURL`). terma-frontend's new `/desktop/*` pages
  are for the desktop apps only; nothing in the CLI links to them.
- Relay service: `internal/relay/service/launchd.go` writes `~/Library/LaunchAgents/ai.terma.relay.plist`;
  `systemd.go` and `windows.go` beside it.

## Changes

### 1. `terma setup --desktop-grant` (hidden flag)

Files: `internal/cli/setup.go` (`setupFlags.desktopGrant`), `internal/cli/signin.go`.

With the flag, `runSetup`:

- Requires `-y`, `--org` and `--team` (ids). `--no-browser` with it is an error. The organization and team prompts
  never run: at onboarding the desktop passes its PERSONAL organization and that organization's only team, and when
  the person switches the CLI later it passes the organization and team picked in the app.
- Refuses when `TERMA_API_KEY` is set (as `signIn` does now).
- The `SignIn` step calls a new `app.signInWithGrant(cmd, cfg, opts)` instead of `setupSignIn`:
  1. Reads the desktop's identity from stdin, one JSON line `{"user_id":"<uuid>","org_id":"<uuid>"}`. `org_id`
     must equal `--org`.
  2. Reuses a stored credential only when it verifies (`reuseStoredSession`'s checks) and both its user id and its
     organization id equal the desktop's. The user id is the verified one (`whoami` fills it for credentials saved
     before terma kept it). Then it prints `{"reused":true}` and no grant is needed.
  3. Else `auth.LoginWithGrant(ctx, exchanger, auth.GrantOptions{Label, Out: stdout, In: stdin})` (new, in
     `internal/account/auth/grant.go`): makes a PKCE pair, writes one JSON line to stdout
     `{"challenge":"<S256>","label":"<label>"}`, reads one JSON line from stdin `{"code":"..."}` (bounded at 8 KiB,
     with a timeout of 2 minutes), then `exchanger.ExchangeDelegatedCode(ctx, code, verifier)` which posts
     `redirect_port: 0`. The verifier never leaves the process. No listener, no browser.
  4. The returned credential's user id and organization id must equal the desktop's; a mismatch is an error
     (`grant_identity_mismatch`), unlike the browser path which only notes it.
  5. Save and apply the login as `signIn` does (`auth.SaveCredential`, `applyLogin`); revoke the replaced session.
     A stored credential for the same organization but another user is replaced this way. Credentials for other
     organizations stay stored.
- Everything after the sign-in step is unchanged: `ChooseAgents` from `--harness`, `SelectTeam` (the given team),
  policy, relay, hooks, check-in. The PERSONAL organization's team starts on Collect All, so at onboarding the policy
  step finds a policy that collects every session.
- The label: `auth.DefaultLabel()` plus " (via Terma Desktop)" so the session list on the web shows which sign-in
  came from the app. The desktop's own session is already labelled "Terma Desktop on <host>".

### 1a. Switching organizations from the desktop

No new command. The desktop switches the CLI with the same `terma setup -y --desktop-grant --org <id> --team <id>`,
using its own desktop session for that organization to delegate. Because the CLI keeps one credential per
organization:

- Onboarding re-points a CLI that collected for a TEAM organization to Personal, and keeps the TEAM credential stored.
- Switching back to that organization finds the stored credential for the same user and organization and prints
  `{"reused":true}`: no grant, no delegate call.
- Switching to an organization the CLI never held asks for a grant as above.

The desktop stops offering `terma switch` for this (it signs out every organization and needs a terminal or browser
to ask). `terma switch` itself is unchanged for people at a terminal.

**What happens to work already in flight when the organization changes** (organization-model council). Existing
routing already keeps tenants apart, so this adds no mechanism; a transition test proves it.

- Queued events keep their team. Each event is queued under the project it was admitted for
  (`hookrun.AttrProjectID`); `delivery.Router` groups by that id, sends with that project's own ingest key
  (`keystore.Get(r.ConfigDir, id)`) and filters with that project's policy (`Router.Policy`). Nothing is retargeted
  to the new team.
- Claimed sessions keep their team: a relay claim carries its `ProjectID` (`internal/relay/claim/claim.go`), and the
  relay resolves policy by the claim.
- Nothing is widened. `routing.EffectivePolicy` applies a team's stored policy only while it belongs to the profile's
  organization ("never borrows another team's grant"). After a move to Personal, the old team's queued events and
  claimed sessions meet `NoPolicy` and are withheld: never sent to Personal, never under Personal's wider policy, and
  dropped. The desktop's notice covers this: the old team stops receiving this Mac's sessions.
- Nothing collected earlier is uploaded because the new default is global: a policy admits sessions from the moment
  it applies, and nothing re-reads past transcripts for it.
- Transition test (`internal/cli/setup_test.go` with the delivery and relay fakes): queue events for team A in
  organization X, hold an active claimed session for A, run the grant setup for Personal, then deliver: no event for
  A is sent with Personal's key or under Personal's policy, and none is sent for A while the profile is on Personal;
  switch back to X (`{"reused":true}`): new sessions for A collect under A's policy again.

### 2. The grant protocol

Pinned in `terma-desktop/shared/fixtures/auth.json` `delegatedGrant`; the CLI tests read the same cases from a copy
under `internal/account/auth/testdata/` (the CLI repo does not read the desktop's fixtures).

- In (stdin, line 1): `{"user_id":"…","org_id":"…"}`. Both are ids and carry no secret.
- Out (stdout, first line, then nothing else until the exchange): `{"challenge":"…","label":"…"}` or
  `{"reused":true}`.
- In (stdin, line 2, only after a challenge): `{"code":"…"}`.
- Errors go to stderr; the exit code is 1; the JSON on stdout for a failure is `{"error":{"code":"…","message":"…"}}`
  as `terma auth token` prints, with codes `grant_no_code` (EOF or timeout before a code), `grant_bad_input` (a
  malformed identity or code line, or `org_id` unequal to `--org`), `grant_rejected` (the auth host refused the code),
  `grant_identity_mismatch`.
- The desktop reads stdout line by line; the first JSON object is the request or the reuse; every later line is
  ordinary setup output. So setup's own output must not start before that line: `runSetup` prints the header after
  the sign-in step when `--desktop-grant` is set, or the header goes to stderr. Decide in the PR; the test asserts the
  first stdout line is JSON.

### 3. The delegated exchange

File: `internal/account/api/token.go`.

`ExchangeDelegatedCode(ctx, code, verifier string) (*auth.Credential, error)` posts
`{"grant_type":"authorization_code","code":…,"code_verifier":…,"redirect_port":0}`. The auth host decides from its
own record of the code whether it is delegated, and accepts port 0 only for a delegated code. `auth.TokenExchanger`
gains the method; the fake exchanger in `login_test.go` implements both.

The CLI never calls `/v1/auth/cli/delegate` and never sees the desktop's refresh token: the desktop calls delegate
with its own refresh token, rotates its own session, and passes only the code.

### 4. Doctor and status

`internal/cli/doctor.go` and `status.go` already name the session label and the team. Add nothing except that
`terma doctor` says "signed in through Terma Desktop" when the label carries the suffix, so a person can tell the
two sign-ins apart.

### 5. Standalone `terma setup` (no desktop)

Unchanged. The platform's changes force nothing here: `/cli/auth` keeps its click and its organization picker for
any organization; a PERSONAL organization shows in the picker and in `/v1/organizations` like any other (the new
`type` field is additive and the CLI's JSON decoding ignores it); a PERSONAL organization has one team, so
`soleOrPick` takes it. A session from `/cli/auth` cannot delegate, and the CLI never delegates.

### 6. Windows install (decided: a PowerShell script first, winget after)

install.sh is bash, so the Kotlin app on Windows needs its own path. Both ship; the script comes first.

- **`install.ps1`** next to install.sh, served at `https://terma.ai/install.ps1`, the same contract as install.sh:
  the latest release from GitHub, checksum verified, into `%LOCALAPPDATA%\Programs\terma`
  (`TERMA_INSTALL_DIR` overrides), PATH added to the user environment (never the machine's, so no admin, and
  `TERMA_NO_MODIFY_PATH` leaves it alone), nothing else written. The desktop runs it as a fixed argv:
  `powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://terma.ai/install.ps1 | iex"`. Tested in CI on
  a Windows runner (`scripts/` has the release checks; add an install smoke test beside the install.sh one).
- **winget** manifest (`Terma.Terma`) submitted to `microsoft/winget-pkgs` from the release workflow once the
  script is out, so people who prefer a package manager get `winget install Terma.Terma`, as Homebrew serves the
  Mac.
- Recommendation: the script first. We control it and it ships the day the Kotlin app needs it; a winget manifest
  waits on Microsoft's review for every release and the app cannot assume winget is present or current on a
  machine. Once the manifest is live, the Kotlin app may prefer winget when it is installed and fall back to the
  script.
- Not an option (decided): bundling a terma binary inside the Kotlin app. The `npm/` package stays for Node users
  only.

## Contract summary

| Surface | Change |
| --- | --- |
| `terma setup` | new hidden `--desktop-grant`; requires `-y`, `--org`, `--team`; stdin line 1 the desktop's identity; stdout line 1 the grant request or `{"reused":true}`; stdin line 2 the code; used for onboarding and for switching from the desktop |
| `POST /v1/auth/cli/token` | the CLI sends `redirect_port: 0` with a delegated code |
| Session label | `<machine> (via Terma Desktop)` |
| Standalone `terma setup` | unchanged |

## Security notes

- The CLI never receives a desktop token and never sees a `TERMA_*` variable from the desktop (the desktop strips
  `TERMA_ENV` and passes `--env`). It receives one 60-second single-use code that only its own verifier can redeem.
- Reuse requires the same user and the same organization, so a CLI signed in as someone else, or to another
  organization, never keeps collecting under the desktop's name.
- The verifier is made in-process and never printed; the challenge is printed once. A log line must never carry the
  code: add it to the `secret` redaction list if setup logs stdin.
- stdin is read with a size bound and a timeout, so a stuck parent cannot hold `terma` open.
- `--desktop-grant` with a terminal attached is allowed (useful for testing) but prints a warning to stderr that
  the identity and code are expected on stdin.
- No new file is written; credentials land where `SaveCredential` puts them (keychain, or the file with
  `--insecure-storage`).
- The flag is hidden like `auth`, `hook`, `launch`: other programs call it, people do not.

## Tests

- `internal/account/auth/grant_test.go`: the protocol cases from the fixture copy (valid, malformed, EOF, timeout,
  second line ignored); the verifier never appears on stdout or stderr.
- `internal/account/api/token_test.go` (new, beside `token_unix_test.go`): `ExchangeDelegatedCode` body has
  `redirect_port: 0`.
- `internal/cli/setup_test.go`: `--desktop-grant` without `-y`, `--org` or `--team` errors; an identity line whose
  `org_id` differs from `--org` fails with `grant_bad_input`; a stored credential with the same user and organization
  prints `{"reused":true}`; the same organization with another user, or another organization, asks for a grant; with a
  fake exchanger and a piped stdin the SignIn step produces a saved credential and the rest of setup runs; a returned
  credential for another user or organization fails with `grant_identity_mismatch`; a switch from organization A to
  B by grant and back to A prints `{"reused":true}` and leaves both credentials stored.
- `internal/cli/command_surface_test.go`: the flag is hidden.
- `test/e2e` (real dev backend, `account.go` has the sign-in helpers): obtain a desktop session through the desktop
  approval route with the existing browser helper, call `/v1/auth/cli/delegate` with its refresh token from the test,
  pipe the identity and the code into `terma setup --desktop-grant -y --harness claude --org … --team …`, and run
  `terma doctor`. Gated on the platform flags in dev.
- `make check` as always.

## Rollout and compatibility

- Additive: a CLI without the flag still works with the desktop, which falls back to the browser approval.
- The desktop checks `terma version` against a floor (the first release with the flag) before using it; install.sh
  always installs the latest, so a fresh Mac gets it.
- No change to hooks, relay, policy or the spool.
- Ship after the platform's delegate endpoint is on in dev; the e2e case gates the release.

## Dependencies

- mirador-platform: `POST /v1/auth/cli/delegate` (refresh-authenticated, for desktop sessions with `can_delegate`,
  which every desktop approval sets, for any organization; the desktop calls it, the CLI never does) and
  `redirect_port: 0` for delegated codes on the exchange. The delegated session takes the delegating session's
  organization, which must equal `--org`.
- terma-desktop: the fixture `delegatedGrant` is written first and copied here. Desktop sign-out never signs the
  CLI out (decided); the app points people at `terma teardown --sign-out`, which already exists. Refresh replay on the
  desktop's session, or "sign out everywhere" on the web, does sign this CLI session out (its delegation family).

## Tasks

| # | Task | Repo | Size | Depends on | Gate |
| --- | --- | --- | --- | --- | --- |
| 1 | `auth.LoginWithGrant` and the protocol (identity line, challenge or reuse, code), testdata copied from the desktop fixture, tests | terma-cli | M | terma-desktop fixture task 1 | Council: approved 2026-11-09 with the changes listed |
| 2 | `api.ExchangeDelegatedCode`, exchanger interface, tests | terma-cli | S | none | Council: approved 2026-11-09 with the changes listed |
| 3 | `setup --desktop-grant`: flag, `signInWithGrant`, reuse on user and organization (switching back included), identity check, stdout ordering, label suffix, tests | terma-cli | M | 1, 2 | Council: approved 2026-11-09 with the changes listed |
| 3a | Organization-change transition test (queued events, a claimed session, switch back) against the existing routing (`delivery.Router`, `routing.EffectivePolicy`, relay claims); add a mechanism only if it fails | terma-cli | S | 3 | Council: approved 2026-11-09 with the changes listed |
| 4 | Error codes on stdout, redaction check | terma-cli | S | 3 | |
| 5 | `doctor` names a desktop-delegated sign-in | terma-cli | S | 3 | |
| 6 | e2e case in `test/e2e` against dev | terma-cli | M | 3, platform tasks 11 to 16 on dev | |
| 7 | `install.ps1` with the install.sh contract, CI smoke test on Windows, served at terma.ai | terma-cli | M | none | |
| 8 | winget manifest from the release workflow | terma-cli | S | 7 | |

## Implementation notes (branch `dawson/unified-onboarding`)

Where the plan was ambiguous or wrong against the code, the implementation took the smallest choice that keeps every
council invariant:

- **1a, queued events are held, not dropped.** The transition test (`TestDesktopGrantCutoverKeepsInFlightWorkWithItsTeam`
  in `internal/cli/setup_test.go`) shows what the existing routing does, with no new mechanism. On Personal, a flush asks
  for team A's policy with Personal's sign-in; the account service answers 403 for a team outside the credential's
  organization (`GET /v1/policy`), so delivery holds A's events under A (`Result.Held`). They are never sent while the
  profile is on Personal, never with Personal's key and never under Personal's policy. They stay queued until the spool's
  `MaxAge` (14 days) drops them, or until the CLI switches back to A's organization before then, when they go with A's key
  under A's policy as it stands at send time (`Router.Allowed` rechecks it). A relay started on Personal resolves the
  claim for A to "no validated collection policy for this team": its records are withheld and dropped after the hold.
  Every invariant holds: in-flight work keeps its team and that team's restrictions, nothing is widened or retargeted,
  and nothing earlier is uploaded under the new policy.
- **Identity line failures are `grant_bad_input`.** EOF, a timeout or an over-long line before the identity line reads as
  a malformed identity. `grant_no_code` stays the code line's.
- **Every failed exchange is `grant_rejected`.** A refused code and an unreachable auth host both end the grant: the
  code is single use, so the desktop starts again either way. The message says which. The contract keeps its four codes.
- **The header goes to stderr** under `--desktop-grant` (the "decide in the PR" choice), so stdout's first line is the
  grant's. Flag misuse (`-y`, `--org`, `--team` ids, `--no-browser`, `--managed-config`, `TERMA_API_KEY`) is an ordinary
  error before anything is read or printed.
- **A code that signs in someone else is also ended.** Besides failing with `grant_identity_mismatch` and storing
  nothing, the CLI revokes that session (best effort) so it does not linger on the web.
- **The label is kept on the credential.** The credential did not record its label, so doctor could not tell a desktop
  sign-in apart. `Credential.Label` (in `credentials.json`, no secret, no new file) is set by the grant only and survives
  refreshes; a browser sign-in for the same organization replaces it.
- **The fixture copy precedes the desktop's.** `internal/account/auth/testdata/delegated_grant.json` is written from this
  plan's and the desktop plan's definition of `delegatedGrant`; copy the desktop's section over it once it lands and
  rerun `go test ./internal/account/auth/`.
