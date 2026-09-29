# Harness compatibility

`internal/compat` is shared by launchers and diagnostics. It maps a harness,
product surface (CLI or desktop), and semantic version to named capabilities.
Callers ask about a capability rather than comparing versions themselves.

The first implemented capability is `codex.no-daemon`. Other flags, hooks, config
formats, and rollout formats should be added when their compatibility differences
are established. This matrix does not currently claim compatibility for those
other features, or change their existing parsers and installers.

## Verified rules

Source audit: 2026-09-29. Endpoints are inclusive. Only stable versions without
custom build metadata use these rules.

| Harness | Surface | Versions | Capability | Result |
| --- | --- | --- | --- | --- |
| Codex | CLI | 0.150.0–0.155.1 | `--no-daemon` | Unsupported |
| Codex | CLI | 0.156.0–0.157.1 | `--no-daemon` | Supported |
| Codex | CLI | Outside those ranges, prereleases, custom builds, unparseable versions | `--no-daemon` | Unknown until probed |
| Codex | Desktop | Any | CLI `--no-daemon` flag | No applicable rule; CLI binary is not probed |

The flag was introduced by [upstream PR #46088](https://github.com/openai/codex/pull/46088).
It is absent from the tagged CLI definitions for
[0.155.0](https://github.com/openai/codex/blob/rust-v0.155.0/codex-rs/tui/src/cli.rs)
and [0.155.1](https://github.com/openai/codex/blob/rust-v0.155.1/codex-rs/tui/src/cli.rs),
and present in
[0.156.0](https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/tui/src/cli.rs),
[0.156.1](https://github.com/openai/codex/blob/rust-v0.156.1/codex-rs/tui/src/cli.rs),
[0.157.0](https://github.com/openai/codex/blob/rust-v0.157.0/codex-rs/tui/src/cli.rs),
and [0.157.1](https://github.com/openai/codex/blob/rust-v0.157.1/codex-rs/tui/src/cli.rs).
Stable tags 0.150.0 through 0.154.0 were also checked for absence.

## Resolution and caching

`ForVersion(Installation)` is pure: no subprocesses, cache access, or inference
from the locally installed program. Telemetry readers can use it with the version
that produced a session, even after the local harness has been upgraded.

`Resolve(ctx, Installation)` resolves a live installation. The caller supplies the
real executable path, bypassing Terma's own shims. On a cache miss it obtains
`codex --version`, applies verified rules, and uses `codex --help` when the result
is unknown. Version and help subprocesses share a 1.5-second budget, within the
launcher's three-second preparation deadline. Results include support, evidence,
and whether the decision came from a rule or a direct probe.

Successful help output without the flag means unsupported. Failure or timeout
means unknown, and the launcher omits the optional flag while keeping its telemetry
arguments. Unknown results are never persisted as negative capability answers.
A successfully detected version may be cached independently of a failed help probe;
the next launch retries the capability probe without repeating version detection.

The cache stores observations (version and any successful help result), not resolved
version rules. A Terma update therefore re-evaluates cached versions with its current
matrix. A direct observation takes precedence over a version rule for the same
installation. Cache files live under `cache/compat-codex-cli` in the Terma config
directory, use atomic writes, and are invalidated by changes to the resolved
executable path, size, modification timestamp, or permissions. Corrupt or unwritable
caches fall back to probing. The previous `cache/codex-capabilities` format is ignored;
its first launch under the new resolver probes again.

`terma doctor` shows the CLI version, selected behavior, decision source, and any
uncertainty. Unknown support for this optional flag is an inconclusive check, not
an assertion that telemetry is broken. Desktop does not borrow a CLI version.

## Adding another compatibility difference

1. Establish the upstream change and examine the release tags immediately before
   and after it. Record source links and the last verified release.
2. Add a named feature and rules scoped to the harness and surface. Keep unknown
   distinct from unsupported; use a named format type when selecting parsers or
   config schemas instead of a boolean flag.
3. Give the feature an explicit unknown policy. Optional launch flags can be omitted;
   consent decisions must not become permissive merely because a version is unknown.
4. Test both sides of every boundary, prereleases, custom builds, and future versions.
   Add representative producer-version fixtures before selecting a telemetry parser.
5. Let consumers use the resolved capability. Keep version comparisons in this package.

Do not extend the upper bound to an unverified future release automatically. For
forks that retain an upstream stable version number, a version rule alone cannot
prove equivalence; a future feature that needs to support such forks should add an
explicit verification probe rather than inventing another release cutoff.
