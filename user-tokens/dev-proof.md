# Dev proof: `terma login` and telemetry on user tokens (miradorlabs/terma-cli#150, #152)

Binary: built from #152's branch (which contains #150) at 69ee735b, as `$SCRATCH/terma-dev`. Environment: `TERMA_ENV=dev`. Config: a throwaway `TERMA_CONFIG_DIR=$SCRATCH/devcfg`. Setup ran with `HOME` set to a scratch directory and the relay service off, so no real agent settings or launchd service were touched. The credentials are in the throwaway directory's plain-text file.

## 1. Sign in (a person approved it in the browser)

```
Open this URL to authorize the CLI:

  https://dev.terma.ai/cli/auth?challenge=…&label=Dawsons-MacBook-Pro&port=50013&protocol=user&response_mode=form_post&state=…

Waiting for authorization...
signed in: user 65c71a0a-8941-4aae-8401-e35f4c2ce134 session 6a1ef6fb-1370-4140-983d-2a9c02630fc8 family 6a1ef6fb-1370-4140-983d-2a9c02630fc8 installation bb5fde8d-150e-458e-9829-d67c89bba599
```

## 2. `terma setup --org "Dawson Test Org" --yes --harness claude --relay-service off` reuses that sign-in

The organization list, team, policy fetch and check-in all go through the one user token:

```


  ✓ Signed in     <developer> (Dawson Test Org)
  ! Credentials   in plain text in $SCRATCH/devcfg/user-credentials.json: no system keychain could be used
  ✓ Agents        Claude Code & Desktop
  ✓ Team          My Awesome Project
  ! Collects      nothing yet: your team has no collection policy
  ✓ Relay         starts when an agent needs it
  ✓ Check-in      this machine reported to your organization
  ✓ Updates       never for a development build: `terma update --force` installs the latest release

! Almost done — finish the steps marked ! below

Next steps:
  • Ask a team admin to set up your team's collection policy in the Terma web app, or, if it was set up on another team, run `terma setup` for that team; until then, nothing is collected.
  • Run `terma doctor` any time to see what terma is collecting and check it end to end.
```

## 3. The relay that setup started sends heartbeats to the dev otel gateway with the user token

The profile has no server key: there's no `keys.json` and `server_key_sign_in` isn't set, so the relay sends as the person, with the bearer user token and `X-Mirador-Project`. Setup started the relay (`--launch hook`). It sent its first beat about a minute later, and a last beat when it was stopped with SIGTERM. `heartbeats_sent` only goes up on a 2xx from the gateway; a refusal would count `heartbeats_failed`.

```
$ ls -a $SCRATCH/devcfg
.
..
config.json
config.json.lock
installation-id
installation-id.lock
policies
relay
setup
spool
user-credentials.json
user-credentials.json.lock

$ cat $SCRATCH/devcfg/relay/stats.json
{
  "since": "2026-11-20T11:12:35.327409-07:00",
  "counters": {
    "heartbeats_sent": 2
  }
}
```

## 4. `terma doctor`

```
Config:      $SCRATCH/devcfg
State:       $SCRATCH/devcfg
Collecting:  nothing yet: your team has no collection policy
Environment: dev (https://auth-dev.terma.ai)
Updates:     never for a development build: `terma update --force` installs the latest release

  warn  terma on PATH             ~/.local/bin/terma (build 4e06df2); hooks run a different build from $SCRATCH/terma-dev
                                  → run `echo 'export PATH="$SCRATCH:$PATH"' >> ~/.zshrc && source ~/.zshrc` to put $SCRATCH first on PATH, or replace ~/.local/bin/terma with this build; then run `terma doctor`
  ok    signed in                 <developer> in Dawson Test Org [dev], credentials in plain text
  warn  repository collected      team "My Awesome Project" (740885d1) in Dawson Test Org (dev) has no collection policy
                                  → Ask a team admin to set up your team's collection policy in the Terma web app, or, if it was set up on another team, run `terma setup` for that team; until then, nothing is collected.
  skip  commit hooks in effect    not a Git repository
  ok    agent exporting to Terma  through the local relay on 127.0.0.1:43999 (running); it forwards the sessions your team's policy collects
  ok    Claude Code status line   capturing plan usage (terma's default line)
  ok    event spool               0 queued, spool writable
  skip  backend receives events   nothing queued at verification time (hooks may already have flushed)

Setup needs attention:
  - run `echo 'export PATH="$SCRATCH:$PATH"' >> ~/.zshrc && source ~/.zshrc` to put $SCRATCH first on PATH, or replace ~/.local/bin/terma with this build; then run `terma doctor`
  - Ask a team admin to set up your team's collection policy in the Terma web app, or, if it was set up on another team, run `terma setup` for that team; until then, nothing is collected.
```

The `terma on PATH` warning is there because the test binary ran from the scratch directory, not `~/.local/bin`, which was left alone on purpose. Collection says "nothing yet" because the dev team has no collection policy; the heartbeat goes regardless.
