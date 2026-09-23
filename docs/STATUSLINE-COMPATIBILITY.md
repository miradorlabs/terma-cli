# Claude status-line wrapping

Terma installs the callback needed for quota capture in the user's settings, even
when no visible status line was configured. Display behavior is deliberately small:

- Existing renderer: execute the saved command with the original stdin, environment
  and working directory. Prefix its nonempty stdout with a small `t ` marker.
- No previous renderer: capture only, with zero stdout and no marker.
- Renderer intentionally emits zero bytes: keep it invisible, with no marker.
- `TERMA_HOOKS=0`: run the original renderer without capture or a marker.

The renderer's bytes after the marker are unchanged. No ANSI stripping, newline
normalization, Unicode conversion, escape parsing, line reflow or trimming occurs.
Only the first output line receives the prefix. Stderr and exit status pass through;
launch failures return nonzero. Cancellation stops the renderer's process group
on Unix; other platforms stop the shell, with no guarantee of descendant termination.
Only `statusLine.command` is replaced: padding, refresh interval, Vim visibility
and unknown options survive, and disconnect restores the previous object.

Terma gives the renderer **30 seconds** even if Claude never cancels it. Set
`TERMA_STATUSLINE_TIMEOUT` in Claude's environment to a positive duration such as
`60s` or `2m` for a slower custom renderer. Missing, invalid, zero and negative
values use the default. A timeout returns exit code **124** and preserves output
already received. Pipe draining is bounded to 100 ms after cancellation or shell
exit, so descendants holding stdout/stderr open cannot keep the wrapper waiting.
On Unix, Terma also kills the remaining group when that drain limit is reached.
Custom renderers should wait for background work whose output they need to display.
Quota capture starts its detached flush before waiting for the renderer; the
renderer deadline does not cancel that flush. The timeout applies with
`TERMA_HOOKS=0` as well.

## Repeatable checks

The ordinary Go suite covers:

- Silent capture with no renderer, recursive-wrapper protection, and empty output.
- SGR styling (bold, dim-compatible resets, italics, underline, inverse,
  strikethrough), 256-color and truecolor foreground/background combinations.
- Powerline/Nerd Font glyphs, CJK, combining marks, emoji and ZWJ sequences.
- OSC 8 links with BEL and ST terminators, cursor controls, carriage returns,
  tabs, CRLF, leading blank lines, missing final newline, invalid UTF-8 and NUL.
- Large output, oversized stdin pass-through, split escape writes, quoted commands,
  pipelines, heredocs, environment/cwd/width preservation, stderr and failure exits.
- Cancellation of background grandchildren, including Terma's own timeout without
  Claude cancellation; capture/delivery before renderer completion; missing-shell
  launch failure.
- Install/restore/idempotency with exotic commands, nested unknown options and
  command text that merely *mentions* `terma hook statusline`.
- Fuzz property: arbitrary renderer bytes are unchanged after the marker, the input
  slice is not mutated, and empty output stays empty.

Run the focused suite with:

```sh
TMPDIR=/private/tmp go test ./internal/harness ./internal/hookrun
TMPDIR=/private/tmp go test -race ./internal/harness ./internal/hookrun
TMPDIR=/private/tmp go test ./internal/hookrun -run '^$' \
  -fuzz FuzzStatusLineIndicatorPreservesRendererBytes -fuzztime 5s -parallel 2
```

`/private/tmp` is for macOS's symlinked temporary directory; on Linux omit TMPDIR.

## Real upstream renderer comparisons

`scripts/test-statusline-compat.py` runs the actual renderers against synthetic
payloads and isolated config paths. It compares direct invocation with the rebuilt
Terma hook, asserting exact stdout (apart from the prefix), stderr and exit status.
Each case is also checked with capture disabled. The Go suite separately verifies
that silent capture spools quota before delivery starts. The runner does not install
either theme into your Claude settings.

Tested dependencies:

- [ccstatusline](https://github.com/sirmalloc/ccstatusline), npm **2.2.29**, published
  tarball SHA-1 `803cabb20d0e3974a5b2dcb0d8154d5f54136ce3`; inspected source revision
  `1c2f718849c7d7bea6be8997fba8e26751468d5c`.
- [claude-statusline](https://github.com/hell0github/claude-statusline), revision
  `8bb8b43df58db2ba7ee8fe7b3d1363f9735e1743`.

The ccstatusline matrix uses standard, Powerline, and multiline hyperlink/Unicode
configurations at 40 and 120 columns with plain and colored markers. The shell
project runs default, truecolor, and bold/256-color themes with directory/context
sections, plus three multi-layer progress-bar states. Its ccusage responses are
local synthetic fixtures; no real account or provider requests are involved.
Neither test proves the theme's billing calculations are correct.

With Node, npm, jq and the local Go toolchain installed:

```sh
make build
mkdir -p /tmp/terma-statusline-deps
npm pack ccstatusline@2.2.29 --ignore-scripts --pack-destination /tmp/terma-statusline-deps
tar -xzf /tmp/terma-statusline-deps/ccstatusline-2.2.29.tgz -C /tmp/terma-statusline-deps
git clone https://github.com/hell0github/claude-statusline.git /tmp/terma-shell-statusline
git -C /tmp/terma-shell-statusline checkout 8bb8b43df58db2ba7ee8fe7b3d1363f9735e1743
python3 scripts/test-statusline-compat.py \
  --terma bin/terma \
  --ccstatusline-js /tmp/terma-statusline-deps/package/dist/ccstatusline.js \
  --claude-statusline-root /tmp/terma-shell-statusline \
  --report /tmp/terma-statusline-compat.json
```

The run passed **23 cases**, each with capture enabled and disabled. The Go
wrapper/install tests and race checks passed; a five-second fuzz run exercised
160,400 inputs without a failure. The OpenCode plugin suite was skipped because
Bun is not installed (unrelated to these status-line renderers).

A run's output hashes document that run only; temporary path names mean they are not
portable golden strings. Each run compares direct and wrapped output in the same
environment.

## Limits of the guarantee

Byte preservation is testable; universal visual equivalence for arbitrary terminal
programs is not. The marker occupies two additional columns on the first line. A
renderer using absolute cursor positioning, carriage returns or backspaces can
move over it; a full-width line or tabs can lay out differently after a prefix.
Terma preserves those control bytes rather than rewriting their meaning. The same
`COLUMNS` and input are passed through; width is not secretly reduced for a theme.
Claude and the terminal ultimately decide which escapes and glyphs they display.

The third-party comparisons test renderer processes and the real Terma binary,
not every Claude/terminal/font combination. They do not claim a new interactive
Claude UI live run. Time-varying, network-backed and random widgets are outside
this deterministic comparison; their command/input/settings remain unchanged.
