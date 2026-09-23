# Instrumentation guide

Terma separates the fast, repository-visible hook path from the binary's runtime and the
backend's usage accounting. This page explains what gets written, what gets sent, and
where to find adapter-specific contracts.

## The hook path

Every hook written by `terma install` is a guarded one-liner that invokes
`terma hook <event>` and exits successfully. The binary owns session files, touched-file
manifests, commit trailers, and spooling, so updating Terma does not require rewriting
logic into every repository.

The guard matters: on a machine without Terma, hooks produce no output, do not fail the
commit, and do not disturb the agent. On a machine with Terma, `TERMA_HOOKS=0` disables
the path. See [DESIGN.md](DESIGN.md) for the reasoning and invariants behind this model.

## Commit attribution

1. Agent hooks announce a session and record files touched by that session.
2. `prepare-commit-msg` intersects staged files with session manifests.
3. Matching sessions receive trailers such as:

   ```text
   Agent-Session-Id: 018f3a2c-7d4e-7a1b-9c3d-2e5f6a7b8c9d
   Agent-Tool: claude-code/2.1
   ```

4. `post-commit` retires committed files and records the commit event.

Several sessions can receive trailers on one commit. Pure human work is left unstamped.
Merge and squash messages are left alone. If a harness cannot report files, a recently
announced active session can claim the commit using the documented four-hour fallback.

Every commit still produces an unstamped count event. It contains commit metadata such as
the SHA, remote URL with credentials removed, branch, author, file count, and line totals,
but not file names or per-file stats unless an agent session participated.

## Queue and performance guarantees

Hooks never wait for the network. They append to a local spool, and a background flush
delivers events after commits and session ends with exponential backoff. Events that arrive
before a project key is available are held rather than dropped.

The product budgets are:

- `prepare-commit-msg` completes in under 50 ms with zero network activity.
- No hook blocks on Terma being reachable.
- The queue is bounded and reports expiration, pruning, malformed input, backoff, and
  unroutable events instead of presenting them as successful delivery.

Useful diagnostics:

```sh
terma spool status
terma spool flush
terma doctor
```

`doctor` proves that the spool can accept a write; merely being readable is not enough.

## Hook managers

`terma install` detects the repository's existing manager and adds only Terma's entries:

| Repository has | Terma adds |
|---|---|
| husky | Lines in `.husky/prepare-commit-msg` and `.husky/post-commit` |
| lefthook | A `terma` command under each relevant hook in `lefthook.yml` |
| pre-commit | A local hook per stage in `.pre-commit-config.yaml` |
| no manager | `.terma/hooks/*` shims and a managed `core.hooksPath` |

`terma uninstall` removes only Terma's lines from shared files and removes files owned by
the repository installation.

## Claude status line

When Claude Code is selected, installation can place `terma hook statusline` in front of
the user's existing status-line renderer. It preserves the original input, options, ANSI
styling, links, and multiline output, while adding a small `t` marker and capturing the
plan's rate-limit windows. `--no-statusline` opts out; disconnect restores the original.

Capture is silent when there is no renderer or when a renderer prints nothing. Renderers
time out after 30 seconds by default; set `TERMA_STATUSLINE_TIMEOUT=60s` when needed.
Funding classification and billing reconciliation happen downstream and remain separate
from this evidence capture. See [STATUSLINE-COMPATIBILITY.md](STATUSLINE-COMPATIBILITY.md).

## Adapter contracts

The detailed, version-pinned contracts live separately so they can evolve with each agent:

- [Cursor instrumentation](CURSOR-INSTRUMENTATION.md)
- [Antigravity instrumentation](ANTIGRAVITY-INSTRUMENTATION.md)
- [Subagent instrumentation](SUBAGENT-INSTRUMENTATION.md)
- [Funding evidence](FUNDING-INSTRUMENTATION.md)

In short, Claude Code and Codex provide native telemetry paths in addition to hooks;
OpenCode uses a dependency-free plugin; Cursor provides ordered observations and optional
token snapshots; and Antigravity provides turn/tool/file observations without token or
cost export. Prompt, response, tool-content, and credential capture are controlled by the
configured consent and scope settings described in [CONFIGURATION.md](CONFIGURATION.md)
and [SECURITY.md](../SECURITY.md).
