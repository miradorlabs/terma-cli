#!/bin/sh
# Times the prepare-commit-msg script a claimed agent session installs into a repository's
# own hooks directory, end to end (sh + terma), in a scratch repository the team policy
# lists and in a linked worktree of it (which shares the one install in the common git
# directory), on a machine routing by its one team and on one moved to routing by teams
# (where the hook reads the profile's routing state and asks Route), and fails when any
# median exceeds the budget. It also times the hooks Claude Code runs around a tool call: the
# post-tool-use hook after an edit, and the pre-tool-use hook before a read and a shell
# command, which runs on nearly every tool call and which Claude Code waits on before the
# call runs, held to the same budget, shell commands at the scan's bounds included. The
# first execution of a freshly written script pays a one-time OS cost (macOS's exec policy
# check) that no commit after it sees.
set -eu
BUDGET_MS="${TERMA_HOOK_BUDGET_MS:-50}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/terma"
[ -x "$BIN" ] || { echo "build first: make build" >&2; exit 1; }

top="$(mktemp -d)"
trap 'rm -rf "$top"' EXIT
export GIT_CONFIG_NOSYSTEM=1 PATH="$ROOT/bin:$PATH"
# What a claimed session's policy says, offline: it lists the repository (which admits its
# worktree too) with git_hooks on.
export TERMA_POLICY_STUB='{"mode":"repo","repositories":["github.com/acme/repo"],"git_hooks":true}'

ms() { perl -MTime::HiRes=time -e 'printf "%d", time*1000'; }

# machine <name> [-migrated]: a scratch machine of its own, set up as terma setup leaves it
# (the relay's token, without which the commit hooks do nothing; the profile's team and its
# validated policy), a repository and a linked worktree of it, and the hooks the installer
# writes into the repository, from the same code. -migrated moves it to routing by teams
# first, as the relay's migration does.
machine() {
  tmp="$top/$1"
  export TERMA_CONFIG_DIR="$tmp/home" TERMA_STATE_DIR="$tmp/state" GIT_CONFIG_GLOBAL="$tmp/gitconfig"
  repo="$tmp/repo"; mkdir -p "$repo" "$TERMA_CONFIG_DIR" "$TERMA_STATE_DIR/relay"
  printf local > "$TERMA_STATE_DIR/relay/token"
  # A relay that just failed is not launched again for a while (daemon.RetryAfter): no hook
  # here starts one, which would migrate this machine mid-bench and outlive the bench.
  : > "$TERMA_STATE_DIR/relay/last-error"
  git -C "$repo" init -q
  git -C "$repo" config user.email bench@example.com
  git -C "$repo" config user.name bench
  git -C "$repo" remote add origin git@github.com:acme/repo.git
  git -C "$repo" commit -q --allow-empty -m init
  git -C "$repo" worktree add -q "$tmp/wt"
  # What the Read and Bash hooks reach: a file in the other checkout of the repository.
  : > "$repo/NOTES.md"; : > "$tmp/wt/NOTES.md"
  # What a shell command at the scan's bound reaches: a file in each of 255 folders.
  i=0
  while [ $i -lt 255 ]; do
    for co in "$repo" "$tmp/wt"; do mkdir -p "$co/many/d$i"; : > "$co/many/d$i/x.go"; done
    i=$((i + 1))
  done
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '{"active_profile":"default","profiles":{"default":{"team":"proj_bench"}}}' > "$TERMA_CONFIG_DIR/config.json"
  mkdir -p "$TERMA_STATE_DIR/policies"
  printf '{"mode":"repo","repositories":["github.com/acme/repo"],"git_hooks":true,"include_prompts":true,"include_tool_content":true,"revision":1,"updated_at":"%s","team_id":"proj_bench","fetched_at":"%s"}' "$now" "$now" > "$TERMA_STATE_DIR/policies/proj_bench.json"
  shift
  (cd "$ROOT" && go run ./scripts/benchhooks "$@" "$BIN" "$repo")
  # One install in the common git directory serves the checkout and its worktree.
  hook="$repo/.git/hooks/prepare-commit-msg"
}

# median <times>: the 4th of 7.
median() { echo "$1" | tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p; }

# tool_hook <hook> <label> <payload> [budget]: times terma's <hook> hook on payload 7 times,
# and fails when a budget is given and the median exceeds it.
tool_hook() {
  ptimes=""
  i=0
  while [ $i -lt 7 ]; do
    start=$(ms)
    printf '%s' "$3" | terma hook --user "$1"
    end=$(ms)
    ptimes="$ptimes $((end - start))"
    i=$((i + 1))
  done
  pmedian=$(median "$ptimes")
  echo "$1, $2: median ${pmedian}ms over 7 runs (${ptimes# })${4:+ — budget ${4}ms}"
  [ -z "${4:-}" ] || [ "$pmedian" -le "$4" ] || { echo "$1, $2: over budget" >&2; exit 1; }
}

# bench <checkout> <slug> <label> <other>: an agent session with a manifest, so the timed path
# includes the staged-files intersection (the expensive branch), not the early exit. The Read
# and Bash hooks reach <other>, a second checkout, so they classify a location besides their own.
bench() {
  dir="$1" sid="018f3a2c-bench-$2" label="$3" other="$4"
  cd "$dir"
  printf '{"session_id":"%s","hook_event_name":"SessionStart","cwd":"%s"}' "$sid" "$dir" | terma hook --user session-start
  mkdir -p src; echo "$label" > src/a.go
  printf '{"session_id":"%s","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"%s/src/a.go"},"cwd":"%s"}' "$sid" "$dir" "$dir" | terma hook --user post-tool-use
  git add src/a.go
  echo "feat: bench" > "$tmp/msg.txt"
  sh "$hook" "$tmp/msg.txt" message
  grep -q "Agent-Session-Id: $sid" "$tmp/msg.txt" || { echo "$label: the hook did not stamp: the timed path is not the real one" >&2; exit 1; }
  times=""
  i=0
  while [ $i -lt 7 ]; do
    echo "feat: bench" > "$tmp/msg.txt"
    start=$(ms)
    sh "$hook" "$tmp/msg.txt" message
    end=$(ms)
    times="$times $((end - start))"
    i=$((i + 1))
  done
  median=$(median "$times")
  echo "prepare-commit-msg, $label: median ${median}ms over 7 runs (${times# }) — budget ${BUDGET_MS}ms"
  [ "$median" -le "$BUDGET_MS" ] || { echo "$label: over budget" >&2; exit 1; }
  # post-tool-use runs the Mixed check (place, the run ledger) on every edit; pre-tool-use
  # runs it before every read and shell command, which only classify what they reach.
  tool_hook post-tool-use "Edit, $label" "$(printf '{"session_id":"%s","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"%s/src/a.go"},"cwd":"%s"}' "$sid" "$dir" "$dir")"
  tool_hook pre-tool-use "Read, $label" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"%s/NOTES.md"},"cwd":"%s"}' "$sid" "$other" "$dir")" "$BUDGET_MS"
  tool_hook pre-tool-use "Bash, $label" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cd %s && cat NOTES.md src/a.go | grep -c bench > /dev/null; ls -la %s/src"},"cwd":"%s"}' "$sid" "$other" "$dir" "$dir")" "$BUDGET_MS"
  # A shell command at the scan's bounds (hookrun's maxShellPlaces and maxShellLookups): a
  # glob reaching 255 folders, and 4000 words that name nothing.
  tool_hook pre-tool-use "Bash, 255 folders, $label" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls many/*/x.go"},"cwd":"%s"}' "$sid" "$dir")" "$BUDGET_MS"
  tool_hook pre-tool-use "Bash, 4000 words, $label" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo%s"},"cwd":"%s"}' "$sid" "$WORDS" "$dir")" "$BUDGET_MS"
}

WORDS="$(i=0; while [ $i -lt 4000 ]; do printf ' w%d' $i; i=$((i + 1)); done)"

machine single
bench "$repo" checkout checkout "$tmp/wt"
bench "$tmp/wt" worktree worktree "$repo"

machine teams -migrated
bench "$repo" teams-checkout "checkout, routing by teams" "$tmp/wt"
bench "$tmp/wt" teams-worktree "worktree, routing by teams" "$repo"
