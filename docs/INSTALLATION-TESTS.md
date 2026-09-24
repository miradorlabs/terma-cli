# Installation and uninstallation tests

Run the real CLI against isolated local fixtures:

```sh
make test-install-e2e
```

The suite builds `terma` once, then invokes it as a subprocess with a timeout for
each command. It uses temporary repositories, independent configuration directories,
and an explicit environment without inherited credentials, exporters, or Git
configuration. It never signs in or requires a live backend. These tests also run
in the normal `make check` suite on Unix (macOS/Linux).

Run a smaller group while developing:

```sh
TERMA_ENV=dev go test ./cmd -run '^TestInstallE2ELocations$' -count=1 -v
TERMA_ENV=dev go test ./cmd -run '^TestInstallE2EUninstallOwnership$' -count=1 -v
TERMA_ENV=dev go test ./cmd -run '^TestInstallE2E(NonGitToGit|TransitionKeepsOtherWorkspaces)$' -count=1 -v
```

## Root selection and non-Git behavior

Inside Git, install/uninstall use Git's worktree root, even from nested directories.
An inner repository or submodule has its own root and never installs into its
parent. A symlink used to enter a workspace resolves to the same physical root.

Outside Git, the first install binds the current directory. Later calls from
subdirectories find the nearest existing `.terma/settings.json` (or legacy
`.terma.toml`). There is no reliable way to infer the intended parent of a fresh,
unbound non-Git folder: run the first install in the directory you want to bind.

Install warns when the current directory or an ancestor contains recognizable
Mercurial (`.hg`), Darcs (`_darcs`), Subversion (`.svn`), CVS (`CVS`), or Bazaar
(`.bzr`) metadata directories. Only Git has version-control integration; agent
hooks and telemetry remain available. Detection is a filesystem heuristic, does
not require those VCS tools, and does not change root selection or modify their
metadata. An actual Git worktree takes precedence over these markers.

Non-Git installs write agent hooks, project binding, and telemetry policy. They do
not create Git hooks, initialize Git, or configure a Git hook manager. Agent
sessions and file touches use private state under the Terma config directory,
keyed by the canonical workspace path. Status and doctor recognize the binding
and skip Git-only checks. Uninstall removes that workspace's private session state.

If that same folder later gains Git, `terma install` adds the Git hooks while
preserving its binding, agent settings, active session, and file-touch history.
The workspace keeps its original private session store rather than copying it:
hooks running immediately after `git init` and before reinstall already use the
same state and lock. The first non-Git install reserves the location even if no
agent events have arrived yet. Reinstalling does not refresh timestamps, duplicate
sessions, or resurrect edits consumed by a commit. Uninstall removes both private
session storage and the Git hook-restoration journal. Other folders and worktrees
bound to the same project keep independent state.

Bare repositories, broken Git metadata, and malformed existing settings fail
without being treated as ordinary unbound folders. Git root output with embedded
newlines is rejected rather than risking installation into the wrong directory.
Configuration files or directories that are symlinks are left untouched with an
error; this is distinct from entering a workspace through a symlink.

## Matrix

| Area | Cases | What is asserted |
| --- | --- | --- |
| Normal roots | Root, five directories deep, spaces/quotes/Unicode/shell metacharacters | Binding and all four agent hook files land at the root; repeated install does not churn the binding; nested uninstall removes them |
| Git layouts | Nested repository, submodule, separate Git directory, linked and detached worktrees, worktree of a bare repository, explicit Git environment | Correct root and metadata directory; no nested accidental binding |
| Worktree isolation | Install linked first; install main before adding linked; uninstall either first | One checkout never changes the other's effective hook path |
| Non-Git folders | Git available, Git missing from PATH, nested repeated install/uninstall | Agent hooks work; no Git hook files or Git metadata are created |
| Other version-control systems | Hg, Darcs, SVN, CVS, Bazaar markers in ancestors; ordinary files with marker names; Git precedence | Install and dry run warn by name; agent hooks remain available; uninstall preserves VCS metadata |
| Non-Git → Git | Existing session, new session before reinstall, first event after `git init`, active-only attribution, rejected install/retry, other workspaces/worktrees | Real commits attribute earlier edits, human-only commits remain unstamped, consumed edits stay consumed, binding and timestamps survive, uninstall cleans both stores without affecting siblings |
| Runtime | Execute the generated Claude SessionStart and PostToolUse commands from a nested non-Git folder | Status sees the session and edited file; uninstall removes its local state |
| Hook managers | Native Git shims, Husky, Lefthook, pre-commit | Existing user hooks/settings survive two installs and uninstall |
| Existing hook paths | Local, inherited global, tilde expansion, explicit empty, changed after install | Custom hook executes once; uninstall restores the value or inheritance, or preserves the later user edit |
| Agent ownership | Claude/Codex matcher groups, Cursor settings, Antigravity named hooks | User commands and unknown settings survive; mixed groups lose only Terma commands |
| Lookalikes and collisions | Text mentioning `terma hook`, user commands named `terma` or `terma-post-commit`, pre-existing shim filename | Names or mentions alone do not authorize replacing/removing user content |
| Telemetry policy | Install journal present, journal missing (another machine), user edits after install | Restore original policy, preserve unproven settings with a message, preserve user edits |
| Safe rejection | Bare repo, broken `.git`, malformed binding/hook JSON, null document/hooks, newline in root path | Error without a new binding or overwritten input |
| Symlinks and permissions | Symlinked agent file/directory or binding directory; existing mode 0600 | External targets and symlinks survive; file permissions are not relaxed |
| Repeat/optional operations | Dry run, `--no-hooks --telemetry=false`, repeat install/uninstall, modified shim after install | Dry run writes nothing; disabled hooks stay absent; modified shims survive uninstall and are reported |

Shared JSON is compared by decoded value; plain user files are compared byte for
byte where formatting is unchanged. Git hook managers are configured through the
CLI, and a custom Git hook is executed by an actual commit. Installed agent hook
commands are also executed through the shell, not merely searched for in files.

## Ownership and remaining state

Uninstall removes known Terma commands and restores values from this machine's
journal. It preserves a telemetry policy whose ownership cannot be proved, user
commands in the same group, unknown settings, and modified/unrecognized shim files. Cursor’s schema version
also remains: an identical value may have existed before Terma, so a version-only
`hooks.json` is harmless and deliberately preserved.
For pre-commit, newly added default hook types carry ownership comments, so only
those additions are removed; older unmarked types are conservatively preserved.

Git's `extensions.worktreeConfig` remains enabled after uninstall: other worktrees
may use it. Terma's hook-path override is removed from the scope where it was
installed. Home-directory routing records and project keys remain because another
workspace may use the same project; `terma shim uninstall` is the separate
machine-wide operation.

## Limits

This is an offline CLI and filesystem suite. It does not launch real coding agents,
exercise their trust dialogs, verify live telemetry delivery, or run third-party
Husky/Lefthook/pre-commit installers. Those have separate integration/live checks.
The subprocess suite is Unix-only; it does not establish native Windows behavior.
Installation uses atomic writes per file, not a transaction across every file:
an I/O failure midway through applying an otherwise valid plan can leave a partial
install. A subsequent install repairs ordinary partial installs; ambiguous or
malformed user files require correction before retrying.
