# Adding a harness

A harness is a coding agent terma connects to Terma: a CLI (Claude Code, Codex, Copilot,
Gemini, OpenCode, Pi, omp, Hermes, dsh, Muse Code), an editor or app agent (Cursor,
Antigravity), or a desktop app that runs one (Claude Desktop, the Codex app). Supporting one
touches three repositories, and the order matters:

1. **terma-cli** captures what the harness does and spools or relays it.
2. **mirador-platform** maps it to AI events.
3. **terma-frontend** shows it, and lists the harness in setup.

The backend ships first, then the frontend, then a terma release that sends the new records.
A terma that sends records the platform cannot map loses nothing; they land in the raw log
plane. But setup must never offer a harness the product cannot show.

## 1. Map the harness before writing code

Answer these from the harness's help, its config schema, its source (shallow-clone into an
empty temp dir and read it; never build or run downloaded code), `strings` on its binary,
and a live run (step 5).

| Question | Why it matters |
|---|---|
| Does it export OpenTelemetry? Which signals, which session key, with prompts and tool content or behind a flag? Can the endpoint point at `127.0.0.1`? | The richest channel: tokens, cost, model, every call. Muse's switches itself off unless it goes to Meta, and carries no content: unusable. |
| Does it read its exporter from a settings file, or only from its environment? | A settings file gets `ConfigureRelay`. An environment-only exporter needs the `terma launch` shell function (`agents.Launched`; the Copilot CLI). |
| Does it run lifecycle hooks? Which events, what JSON on stdin, user-level or repository-level, trusted without approval? | Hooks claim the session, attribute commits and, for hooks-only harnesses, are the whole export. |
| Does it have a plugin or extension API that sees prompts, messages and tool calls? | A terma plugin can export through the relay (OpenCode, Pi, omp, Hermes, dsh). Check what it receives, such as images and data URLs. |
| How does a subagent appear? | Child sessions, an `agent_id`, or a root id in the environment (Muse's `MUSE_SESSION_ID`). Records must land in the root session. |
| Are there hidden side threads (titles, summaries, reminders)? | They fire hooks and make calls. They are real cost, but must not read as the person's work. |
| Where do sessions live? | For completeness only: terma never reads them to fill a gap. The documented exceptions are Codex's rollout (replies, images) and Claude Code's transcript (call responses, titles, images). |
| How is a build fetched by version, and is the download gated? | The e2e suite and the Harness Reference need per-version installs. Muse's download needs a Meta login, so CI cannot fetch it. |
| Does it have an offline or echo provider? | It lets the live check run with no account. Muse has `--provider echo`. Otherwise use the fixture provider in `test/e2e`. |
| Desktop app: how does it pin and start its agent? | `desktop.go` reads the pin from the app bundle. The pin's shape changes between releases (Claude Desktop 4.23118.1 moved to release candidates). |

## 2. Choose the channels

| The harness has | terma uses | Examples |
|---|---|---|
| OTLP with content, endpoint in a settings file | The relay: `RelayExporter`, plus a `Correlator` and `Capturer` (`internal/relay/shape`), plus hooks for the claim and commits | Claude Code, Codex, Gemini |
| OTLP read only from its environment | `Launched`: setup's shell function starts it through `terma launch <name>` | Copilot CLI |
| A plugin or extension API | terma's plugin, exporting OTLP to the relay and calling `terma hook <event>` for the claim | OpenCode, Pi, omp, Hermes, dsh |
| Hooks only (no usable OTLP) | Hook events spooled and delivered: `terma.session.start`, `terma.tool.call`, `terma.files.touched`, `terma.user.message`, `terma.assistant.message`, `terma.model.call`, `terma.session.observation` | Antigravity, Muse Code, Cursor |
| A session API beside its hooks (timings, titles, its plan's usage) | A detached `terma hook <agent>-feed` that a turn's end starts (`hookrun.Env.Detach`), reading the API through a short-lived process of the agent's own; hooks still claim the session | Muse Code (MSP over `muse serve`) |
| A desktop app that runs a CLI agent | `Surfaced`: the app is a surface of the agent, its pin tested as that agent's build | Claude Desktop, Codex app |

Prefer the channel the harness already runs. A user-level setting reaches the IDE extensions
and desktop apps too; a repository file reaches only the CLI, and touching a repository's
working tree is forbidden anyway (AGENTS.md, Rules).

## 3. terma-cli

### The agent package

`internal/agents/<name>/` is the only place that names the harness (`internal/boundary`
enforces it). One line in `internal/agents/builtin` registers it.

- **Always:** `Agent`: `Name`, `DisplayName`, `Installed`, `Events` (hook event → handler),
  and `FlushAfter` (the events that end a turn or session, after which delivery runs).
- **Optional interfaces, by channel:**
  - `UserHooks`: `UserHooksPath` and `PlanUserHooks`, written with `hookmgr.MergeEventHooks`.
    Use `HooksFile.Defaults` for members a new file needs (Muse's `schema_version`).
  - `UserHooksTrust` / `HookTrusting`: hooks that need the developer's approval.
  - `ManagedHooks`: an organization deploys the hooks.
  - `RelayExporter` and `Exporting`: settings-file OTLP. `Launched`: environment-only OTLP.
  - `PayloadReader`: when the payload's session key isn't `session_id`, or the root session
    comes from elsewhere.
  - `Correlator` / `Capturer`: the relay's session keys and content rules.
  - `ContentConsent`, `StateKeeper`, `StatusLiner`, `RelayChecker`, `EmissionChecker`,
    `Surfaced`, `Covered`: see `internal/agents/*.go`, each documented.
- **`Coverage()`** says, in the words `terma doctor` prints, what is and isn't collected.
- **Setup:** `Support(...)` in `builtin` offers it. Everything else shows as "Coming Soon".

### Hooks

- An entry only runs `[ -x terma ] && terma hook --user <event> || true`
  (`hookmgr.UserHookCommand`). All logic is in the binary.
- A hook prints nothing unless the harness requires a reply. Some harnesses pass a prompt
  hook's output to the model, and some expect `{}`.
- Hook event names are wire contracts: `internal/agents/builtin/registry_test.go` pins them.
  The hooks file the agent writes is pinned byte for byte in `internal/contract/testdata`;
  accepting a new one (`-update`) is a release decision.
- Make heavy hooks asynchronous where the harness allows it (Muse's PostLLMCall carries the
  whole conversation).
- File every record under the root session. A subagent is `terma.agent.id`, never a session
  of its own.

### Records and the registry

Every event and attribute terma sends is in `semconv/registry` (Weaver). Add new ones there,
`make semconv` (CI pins weaver's version in `ci.yml`), and use the generated constants.

- Reuse before adding: `terma.tool.call`, `terma.files.touched`, `terma.user.message`,
  `terma.assistant.message`, `terma.model.call`, `terma.session.observation` and
  `terma.prompt.image` already cover most hooks-only harnesses.
- **Content** (what was said, a tool's input and output) carries a `content` annotation and
  goes in `semconv/policies/terma.rego`'s `content_keys`.
- **Delivery** must classify every new hook event kind, or it is withheld whenever the policy
  withholds any content:
  - `hookContent`: tool-content keys stripped by the policy;
  - `contentFree`: no content, or sent whole;
  - `Allowed`: prompts-gated kinds, sent only while the team collects prompts and the agent
    consents.
- Spool what was said only under the developer's consent (the agent among their harnesses,
  or global mode), and bound it (16 KiB for text). Delivery checks again.
- Large bytes stay out of the spool, which holds 16 MiB in all. Pasted images, for example,
  wait in the state directory's `images/` and delivery sends one per request.
- Referencing an OpenTelemetry attribute with an enum renders its members. `gen_ai.provider.name`
  names `gcp.gemini`, which `internal/boundary` reads as naming an agent: leave such
  attributes out, or classify the member.

### The relay

- `Correlation.SessionKeys`: which attribute names the session, ranked. Mark a key `Claimed`
  when the agent's hooks claim every session it names, so hidden side threads wait for a
  claim in global mode.
- `CaptureRules`: prompt and tool-content fields, events and markers, plus the `SafeKeys` that
  pass with content withheld. An attribute in neither list is reported unclassified in the
  nightly; classify it rather than let it through.

### Checks before committing

- Handler tests with recorded payloads (`hookruntest`).
- `make check`. Use the toolchain's `gofmt`, `$(go env GOROOT)/bin/gofmt`: an older one on
  `PATH` disagrees with CI.
- **A live check:** the real binary, in a temp `HOME` and XDG dirs, with auto-update off, an
  offline provider and terma's hooks written by the real planner. Put a logging wrapper in
  place of terma's binary to see which hooks fire and with what payload. Unit tests can't
  tell you whether the harness accepts the hooks file (Muse rejects unknown fields,
  lowercase event names and array commands).

## 4. The e2e suite, the matrix and the Harness Reference

- **`test/e2e/<harness>.go`:** fetch a build by version (`resolve` with a release list and an
  `ensure`), and a `forEach<Harness>` that skips under `TERMA_E2E_ONLY` when the harness
  isn't named. A harness only found on `PATH` (Pi, omp, Hermes, T3) is tested as installed.
- **Contracts:** scenarios against a fixture provider. Each calls
  `ProvesAll(t, b, capabilities...)` with ids from `compat.go`'s list; add a capability
  there with its label.
  - The compat matrix and each build's verdict come from these rows:
    - **supported:** everything recorded passes;
    - **degraded:** something else failed;
    - **unsupported:** `relay.telemetry` or `telemetry.native` failed;
    - **untested:** no rows at all.
  - Use `SkipBuild` for a build the driver cannot drive: that is untested, not failed.
- **Profiles and probes** (`Test<Harness>Profile`, `…Probe`) record what the build sends,
  stores and runs, for the Harness Reference.
- **Wiring:**
  - `live.yml`, the nightly: the harness's versions env (`checked` tests every build since
    the last one checked), its install steps and secrets.
  - `reference/new-builds.sh`: its registry and package, so the 15-minute check
    (`reference.yml`) captures new builds and runs their contracts.
  - `refgen/meta.go`: its name, vendor, homepage, changelog and source.
  - A desktop app also needs its feed (`desktop_feeds.go`) and pin.
- **Gated downloads** (Muse): no CI capture. Say so in the harness's coverage, and keep a
  local e2e test that runs against the installed build.

## 5. mirador-platform

- **OTLP:** a harness that exports OTLP needs an adapter under
  `gateways/otel/internal/mapping/aisignal/adapter/<source>` (or a shared one: `openrouter`
  maps OpenCode's).
- **Hook events:** the `termacli` adapter maps them. Admit the agent explicitly, since an
  unvetted agent is logged and dropped:
  - `termaHookOnlySources` with an observation mapper;
  - `termaReplySources`, `termaTitleSources`, `termaImageSources`;
  - `termaVettedSources`.
- Content keys go in `termaPromoted`, so content is only ever a ContentPart, never an
  attribute.
- Event types and subtypes are closed vocabularies (`core/ai/event_type.go`); a new one
  updates the proto comments, the taxonomy doc and the frontend list.
- The platform rules apply: an integration test for a changed handler; `/mirador:council`
  for contract changes; schema changes under the four-eyes rule.
- Document each record in `adapter/termacli/README.md`'s table.

## 6. terma-frontend

- `telemetry/sources.ts`: the harness's `TELEMETRY_SOURCES` entry, the only place that
  names it.
- `SETUP_TOOLS`: only once setup can set it up.
- The `/cli` page and `public/cli/llms.txt` follow terma's help; update them if commands
  change.
- The privacy page, if the harness brings a new kind of data (move its `effective` date).
- The Harness Reference pages render from the published reference; nothing to add.

## Checklist

- [ ] Surfaces mapped (section 1), live-checked with an offline provider.
- [ ] `internal/agents/<name>` with tests; registered; `Support` if setup offers it.
- [ ] Event names and the hooks file's snapshot accepted.
- [ ] Registry: new events and attributes, content annotated, delivery classified.
- [ ] Relay shape (OTLP harnesses): session keys, capture rules, no unclassified keys.
- [ ] `make check`.
- [ ] e2e: version fetching, contracts with capabilities, profiles; `live.yml`,
      `new-builds.sh`, `refgen/meta.go`.
- [ ] Platform adapter mapped, vetted, documented, integration-tested.
- [ ] Frontend source entry, setup tools, privacy page if needed.
- [ ] Ship platform, then frontend, then terma.
