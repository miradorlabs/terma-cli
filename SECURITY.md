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
| Project server key `ter_srv_` | until revoked | `~/.config/terma/keys.json` (0600); direct exporters may also keep it in agent config or a headers helper | Write telemetry into the one project it is bound to |
| Local relay token | until replaced or removed | `~/.config/terma/relay/token` (0600) and agent config, helpers, or extensions | Submit exports to the local relay; forwarding still depends on collection policy and routing |

A CLI credential is **org-scoped**. A server key is **project-scoped and write-only for
telemetry**; the relay and hook spool deliver with the destination project's key.
The local relay token authorizes requests to the relay, not to Terma's backend.
These credentials are never written into a repository: `terma install` produces hook
wiring and `.terma/settings.json` (project id and hook installation metadata) in the
repository. The agent hook files
themselves record which adapters are wired. Install also writes per-developer
keys, collection policies, and routing configuration under the user's configuration
directory, updates the selected agents' user settings or extensions, and can install a
per-user relay service. It edits no shell startup file.
The paths below use the default configuration directory; `XDG_CONFIG_HOME` and
`TERMA_CONFIG_DIR` can change it.

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
| API, spool, and relay upstream clients refuse redirects | A redirect replaying credentials or telemetry to another destination |
| The approval page validates the link's shape and builds the redirect itself | An open redirect through `/cli/auth` |

## Hooks: what they can and cannot do

- **Never fail a commit.** Every committed hook invocation guards the binary and ends
  with `|| true`; a missing or broken `terma` is ignored. `prepare-commit-msg` uses local
  session state, updates the commit message, and queues its event locally. It makes no
  backend request and has a 50 ms budget CI enforces.
- **Never block on the backend.** Hooks append to a local spool. Delivery is a detached
  background process with exponential backoff. Flushes prune expired events and
  discard the oldest events over the queue's disk bound; see the retention rules below.
- **Committed files are inert without the binary.** The shims, `.claude/settings.json`
  hooks, and `.terma/settings.json` contain no secrets and do nothing on a machine without
  `terma`.
- **Chaining is bounded.** A shim chains the repository's own `.git/hooks/<name>` (or
  `TERMA_CHAIN_HOOKS_DIR`), never `core.hooksPath`, so it cannot recurse into itself.
- **Hook capture is bounded and selective.** Hooks record session ids, edited file
  paths, tool names, timings, and allowlisted account, credential presence, plan, and
  quota metadata. Edit handlers inspect the path fields of tool input; they do not
  export the entire input or output. Native exporters and Terma's agent extensions
  provide conversation and tool content through the relay, under the policy below.
- **Codex replies and titles are additional content reads.** End-of-turn capture reads
  assistant messages from Codex's rollout because its native export omits them. The
  rollout open is confined to `CODEX_HOME`, rejects symlinks and non-regular files, and
  bounds each invocation to 32 messages and a 1 MiB tail, with 16 KiB per message.
  Session titles are read separately from `CODEX_HOME/session_index.jsonl` and capped
  at 256 bytes. These captures require prompts and logs to be allowed by team policy,
  no excluded paths, and the applicable Codex content consent. With the relay enabled,
  a repository's routing record must enable Codex, logs, and prompts; global mode can
  capture without a routing record. Legacy direct configurations use Codex's native
  settings and honor a saved repository opt-out. An unreadable consent source refuses
  capture. `--prompts off` (also `--exclude-prompts`) withholds replies and titles too.
  The hook does not export user prompts or tool output from the rollout, or use a hook
  payload's last-assistant-message field. Replies and titles wait in the hook spool.
- **Every commit is counted, not catalogued.** `post-commit` spools an event for each
  commit so coverage can be measured. For a commit no agent session was stamped into,
  that event carries the commit's identity and size only — sha, remote URL with
  credentials stripped, branch, author email, file count, lines added and deleted — and
  never file names or per-file stats; those appear only on commits that carry a session
  trailer. Merge and squash message sources are not stamped; unstamped merges and
  recognized squash commits are omitted from the coverage count.

## The local relay and collection policy

`terma setup` and `terma install` point selected agents at a local OTLP/HTTP relay,
by default `127.0.0.1:43180`. Claude Code, Codex, and Gemini CLI use their user-level
exporter settings; OpenCode, omp, Pi, Hermes, and DeepSeek Harness use Terma's plugins
or extensions. Relay setup enables content in machine-level exporter configuration so
the relay can enforce different choices for each destination. Turning prompts off for
a project prevents forwarding them; it does not guarantee that content never reaches
the local relay.

The relay requires its local bearer token for exports and status requests. Gemini's
settings have no headers field, so its endpoint carries the token in the URL path.
The local connection uses cleartext HTTP. The default listener is loopback, but the
advanced `--addr` override can bind another interface; it is not forced to loopback.
Exposing that listener also exposes its HTTP traffic to that network.

Collection has two modes, selected by the signed-in team's policy:

- **Repository mode (the default):** hooks in a bound workspace claim sessions for its
  project. Records are routed by session or trace evidence and, when a claim names
  processes, must come from a covered sender. An unidentified sender does not satisfy
  such a claim. Claims without process information cannot enforce that sender check.
  Sessionless records can be attributed by process only after it exits, when it named
  exactly one claimed session. Unplaced records wait in bounded memory and expire;
  they do not go to the outbox or upstream. The default hold is two minutes, or
  30 minutes for traces waiting for a session.
- **Global mode:** all native exports, including sessionless records and work outside
  installed repositories, go to the selected team's project, marked as catch-all
  attribution. Repository opt-in is not required. Team content and signal limits
  still apply.

`terma setup` and every real `terma install` fetch policy with a developer login,
including hooks-only installs and `--harness none`. A telemetry key cannot authorize
that policy request. Validated policies are cached by team, organization, and auth
environment; the relay refreshes them in the background, normally once per minute.
Without a validated policy, forwarding waits for a successful fetch. A failed fetch,
including a denied request, retains the last validated policy for at most seven days
after it was fetched; after that it grants nothing until a fetch succeeds. A failed
refresh is therefore not an immediate revocation of existing capture. Invalid
responses, older revisions, and another team's policy cannot widen the cached grant.

Team policy limits signals, prompts, tool content, and file paths. A project's local
`routing/<project-id>.json` can only narrow those limits. Prompts and tool content are
on for a first install unless team policy forbids them; reinstalling keeps the saved
choices unless flags change them. `--prompts off` withholds prompt text, model replies,
and titles; `--exclude-tool-content` withholds tool parameters, input, and output.
With either content class withheld, the relay drops unclassified textual attributes,
filters known content fields, and removes free-text log bodies and provider error
messages. It counts unclassified keys locally so compatibility checks can detect new
fields. This is a compatibility rule over known exporter shapes, not a general secret
scanner. Nonempty path exclusions also disable free-text capture because exporters
cannot reliably name the source files of arbitrary text.

The relay applies policy before writing its outbox and again before sending queued
exports. Spool delivery also rechecks signals, paths, coverage, and the consent for
Codex replies and titles. Tightening policy can withhold already queued content from
delivery; it does not immediately erase the bytes already on disk.

Hooks start the relay when needed; a per-user service can keep it available before an
agent's first hook. The relay also sends organization-level health heartbeats using
the developer login: a random machine id, versions, settings, delivery and loss counts,
and queue sizes, without conversation content. Failed heartbeats are not queued.

Advanced `terma connect` can configure an exporter to send directly upstream.
Relay claims and redaction protect only traffic sent through the relay; a direct
exporter follows its agent's own capture settings. Relay installation passes no project
key in Codex launch arguments.

## Content stored on the machine

Terma can keep permitted conversation and tool content in both delivery queues:

| Store | What it holds | Bounds |
|---|---|---|
| `~/.config/terma/spool/events.jsonl` | Hook events, including consented Codex replies and titles | 16 MiB and 14 days, enforced when a flush prunes the queue |
| `~/.config/terma/relay/outbox/` | Exports admitted by collection policy, filtered before being queued | 256 MiB and 14 days, enforced by the running relay's sweeper |
| `~/.config/terma/relay/outbox/.dead/` | Exports set aside after a permanent upstream refusal | 32 MiB and 14 days, enforced by the same sweeper |

These are plaintext JSONL or protobuf files, created with mode 0600 in private
directories (0700); they are not encrypted by Terma. Pruning requires a flush or a
running relay, so a stopped installation does not erase expired files on a timer.
Queue writes use atomic replacement where needed but do not fsync every event: a
power cut can lose recent records. Unplaced relay records remain in memory, bounded
to 50,000 records and 64 MiB, and are lost on restart. The agents' own transcripts and
logs have separate retention rules that Terma does not control.

## Identity and attribution are not authentication

The `Agent-Session-Id` / `Agent-Tool` trailers, the `enduser.id` a harness sends, and the
session events the spool delivers are **attribution among colleagues, not a security
boundary**. Anyone with commit access can write any trailer by hand; anyone can set any
identity in their harness config. Terma treats them as *who to bill the work to*, not as
proof of who did it. Outcomes that need to be trustworthy (what merged, what was reverted,
what CI said) come from the GitHub App, which reads the trailers back from commits it can
verify came through GitHub.

File manifests record that a session touched a file, not which lines it authored or
how its spend should be divided among commits. A fresh active session with no manifest
can also supply fallback attribution. Trailers and line counts are evidence of work,
not an exact measurement of agent authorship.

## What this does *not* protect against

- **Anything running as your user.** It can read credentials, project keys, relay tokens,
  queued content, and agent settings, or change local claims and cached policies.
  File permissions stop other users, not your own code. If you run untrusted code,
  `terma logout` and rotate the project keys in the Terma app. The local relay token
  and process checks do not make hostile code running as you trustworthy.
- **Root, or anyone who can read your disk.** Full-disk encryption is the control.
- **Immediate revocation from a failed policy refresh.** The last validated policy can
  remain active for up to seven days during outages or denied refreshes. Revoking a developer login and
  revoking project telemetry keys are separate actions; cached policy is not proof
  that the developer still has access.
- **A hook kill switch as a telemetry pause.** `TERMA_HOOKS=0` stops hook capture and
  stamping, while preserving the previous status-line renderer and Codex notifier.
  It does not stop native exporters, the relay, existing claims, or queued delivery.
- **Complete machine removal through repository uninstall.** `terma uninstall` removes
  that repository's wiring and binding; machine-level keys, routing, exporters, and
  the relay service are separate. `terma relay daemon remove` removes the service,
  but remaining hooks can start the relay again. The testing reset `terma nate` does
  not currently remove every relay extension, Gemini setting, or service definition.
- **A malicious browser extension.** It can watch you approve; it cannot redeem the code.
- **A hostile repository.** `terma install` writes files you review in a PR; but a
  repository that already carries a malicious `.terma/hooks/*` runs it like any other
  hook once `core.hooksPath` points there. A project binding is a routing reference,
  not proof that repository code is safe; review the hooks like any other code. Agent
  trust decisions are made in the agent, and Terma never approves Codex hooks for you.

## Reporting

Do not open a public issue for a vulnerability. Contact the Terma team at
security@terma.ai.

## For reviewers

Properties worth re-checking after any change:

- The verifier never appears in a URL, a log, or a file (`internal/account/auth` tests).
- No credential or key is rendered by `-o json` (`config show`, `status`, `doctor`).
- `prepare-commit-msg` makes no network call and stays under budget (`make bench-hook`).
- Manifest attribution uses touched staged files, and an empty manifest does not fall
  back to the active session (`internal/session` and `internal/hooks/hookrun` tests).
- The event for an unstamped commit names no file and carries no credential
  (`internal/hooks/hookrun` `TestPostCommitOnAnUnstampedCommitEmitsOnlyACount`).
- The shim never chains itself (`internal/hooks/hookmgr` `TestShimNeverChainsItself`).
- Events without a project key are held, not dropped (`internal/spool` held-event test).
- Relay exports require the local token, and repository-mode records satisfy the
  session/trace claim and sender checks (`internal/relay` and `internal/relay/claim`).
- Unplaced repository-mode records never reach disk or upstream; global mode is an
  explicit policy choice (`internal/relay` outbox and routing tests).
- Withheld content is filtered across attributes, log bodies, scope/link attributes,
  and provider error messages; unclassified textual attributes are dropped
  (`internal/relay` allowlist and capture-policy tests).
- Queued native exports, Codex replies, and titles obey the current delivery policy
  (`internal/relay`, `internal/hooks/hookrun`, `internal/agents/codex` and `internal/cli` capture-policy tests).
- Policy fetches use developer authentication, reject incomplete or older policies,
  and keep teams and environments separate (`internal/account/api`, `internal/routing`,
  `internal/relay/daemon` and `internal/cli` policy tests).
- Codex rollout reads remain confined and bounded; replies and titles require their
  applicable content consent (`internal/agents/codex` and `internal/hooks/hookrun` tests).
- Queue permissions and the spool, outbox, and refused-export bounds match the claims
  above (`internal/spool` and `internal/relay` tests).
- No project id becomes a filesystem path or script name without validation
  (`internal/project` `ValidID`, mirrored in the OpenCode plugin).

`govulncheck` runs in CI on every push and weekly.
