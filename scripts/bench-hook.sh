#!/bin/sh
# Times the prepare-commit-msg script a claimed agent session installs into a repository's
# own hooks directory, end to end (sh + terma), in a scratch repository the team policy
# lists and in a linked worktree of it (which shares the one install in the common git
# directory), on a machine routing by its one team and on one moved to routing by teams
# (where the hook reads the profile's routing state and asks Route), and fails when any
# median exceeds the budget. The first execution of a freshly written script pays a
# one-time OS cost (macOS's exec policy check) that no commit after it sees.
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
  git -C "$repo" init -q
  git -C "$repo" config user.email bench@example.com
  git -C "$repo" config user.name bench
  git -C "$repo" remote add origin git@github.com:acme/repo.git
  git -C "$repo" commit -q --allow-empty -m init
  git -C "$repo" worktree add -q "$tmp/wt"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '{"active_profile":"default","profiles":{"default":{"team":"proj_bench"}}}' > "$TERMA_CONFIG_DIR/config.json"
  mkdir -p "$TERMA_STATE_DIR/policies"
  printf '{"mode":"repo","repositories":["github.com/acme/repo"],"git_hooks":true,"include_prompts":true,"include_tool_content":true,"revision":1,"updated_at":"%s","team_id":"proj_bench","fetched_at":"%s"}' "$now" "$now" > "$TERMA_STATE_DIR/policies/proj_bench.json"
  shift
  (cd "$ROOT" && go run ./scripts/benchhooks "$@" "$BIN" "$repo")
  # One install in the common git directory serves the checkout and its worktree.
  hook="$repo/.git/hooks/prepare-commit-msg"
}

# bench <checkout> <slug> <label>: an agent session with a manifest, so the timed path
# includes the staged-files intersection (the expensive branch), not the early exit.
bench() {
  dir="$1" sid="018f3a2c-bench-$2" label="$3"
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
  median=$(echo "$times" | tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p)
  echo "prepare-commit-msg, $label: median ${median}ms over 7 runs (${times# }) — budget ${BUDGET_MS}ms"
  [ "$median" -le "$BUDGET_MS" ] || { echo "$label: over budget" >&2; exit 1; }
}

machine single
bench "$repo" checkout checkout
bench "$tmp/wt" worktree worktree

machine teams -migrated
bench "$repo" teams-checkout "checkout, routing by teams"
bench "$tmp/wt" teams-worktree "worktree, routing by teams"
