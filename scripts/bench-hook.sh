#!/bin/sh
# Times prepare-commit-msg and Read/Bash/Grep pre-tool-use hooks end to end across a
# checkout and linked worktree, both single-team and routed (routing.State), failing when
# any median exceeds BUDGET_MS (default 50ms).
set -eu
BUDGET_MS="${TERMA_HOOK_BUDGET_MS:-50}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/terma"
[ -x "$BIN" ] || { echo "build first: make build" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export GIT_CONFIG_NOSYSTEM=1 PATH="$ROOT/bin:$PATH"
export TERMA_POLICY_STUB='{"mode":"repo","repositories":["github.com/acme/repo"],"git_hooks":true}'

ms() { perl -MTime::HiRes=time -e 'printf "%d", time*1000'; }
median() { echo "$1" | tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p; }

tool_hook() {
  ptimes="" i=0
  while [ $i -lt 7 ]; do
    start=$(ms); printf '%s' "$3" 2>/dev/null | terma hook --user "$1"; end=$(ms)
    ptimes="$ptimes $((end - start))"; i=$((i + 1))
  done
  pmedian=$(median "$ptimes")
  echo "$1, $2: median ${pmedian}ms over 7 runs (${ptimes# })${4:+ — budget ${4}ms}"
  [ -z "${4:-}" ] || [ "$pmedian" -le "$4" ] || { echo "$1, $2: over budget" >&2; exit 1; }
}

bench() {
  cd "$1"
  sid="018f3a2c-bench-$2"
  printf '{"session_id":"%s","hook_event_name":"SessionStart","cwd":"%s"}' "$sid" "$1" | terma hook --user session-start
  mkdir -p src; echo "$2" > src/a.go
  printf '{"session_id":"%s","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"%s/src/a.go"},"cwd":"%s"}' "$sid" "$1" "$1" | terma hook --user post-tool-use
  git add src/a.go
  echo "feat: bench" > "$tmp/msg.txt"
  sh "$hook" "$tmp/msg.txt" message
  grep -q "Agent-Session-Id: $sid" "$tmp/msg.txt" || { echo "$2: the hook did not stamp: the timed path is not the real one" >&2; exit 1; }
  times="" i=0
  while [ $i -lt 7 ]; do
    echo "feat: bench" > "$tmp/msg.txt"
    start=$(ms); sh "$hook" "$tmp/msg.txt" message; end=$(ms)
    times="$times $((end - start))"; i=$((i + 1))
  done
  m=$(median "$times")
  echo "prepare-commit-msg, $2: median ${m}ms over 7 runs (${times# }) — budget ${BUDGET_MS}ms"
  [ "$m" -le "$BUDGET_MS" ] || { echo "$2: over budget" >&2; exit 1; }
  tool_hook pre-tool-use "Read, $2" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"%s/NOTES.md"},"cwd":"%s"}' "$sid" "$3" "$1")" "$BUDGET_MS"
  tool_hook pre-tool-use "Bash, $2" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cd %s && cat NOTES.md src/a.go | grep -c bench > /dev/null; ls -la %s/src"},"cwd":"%s"}' "$sid" "$3" "$1" "$1")" "$BUDGET_MS"
  tool_hook pre-tool-use "Grep, $2" "$(printf '{"session_id":"%s","hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"TODO","path":"%s"},"cwd":"%s"}' "$sid" "$1" "$1")" "$BUDGET_MS"
}

run_mode() {
  mode="$1"; shift; dir="$tmp/$mode"
  export TERMA_CONFIG_DIR="$dir/home" TERMA_STATE_DIR="$dir/state" GIT_CONFIG_GLOBAL="$dir/gitconfig"
  repo="$dir/repo"; mkdir -p "$repo" "$TERMA_CONFIG_DIR" "$TERMA_STATE_DIR/relay" "$TERMA_STATE_DIR/policies"
  printf local > "$TERMA_STATE_DIR/relay/token"
  git -C "$repo" init -q && git -C "$repo" config user.email bench@example.com && git -C "$repo" config user.name bench
  git -C "$repo" remote add origin git@github.com:acme/repo.git && git -C "$repo" commit -q --allow-empty -m init
  git -C "$repo" worktree add -q "$dir/wt"; : > "$repo/NOTES.md"; : > "$dir/wt/NOTES.md"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '{"active_profile":"default","profiles":{"default":{"team":"proj_bench","server_key_sign_in":true}}}' > "$TERMA_CONFIG_DIR/config.json"
  printf '{"mode":"repo","repositories":["github.com/acme/repo"],"git_hooks":true,"include_prompts":true,"include_tool_content":true,"revision":1,"updated_at":"%s","team_id":"proj_bench","fetched_at":"%s"}' "$now" "$now" > "$TERMA_STATE_DIR/policies/proj_bench.json"
  (cd "$ROOT" && go run ./scripts/benchhooks "$@" "$BIN" "$repo")
  hook="$repo/.git/hooks/prepare-commit-msg"
  bench "$repo" "$mode-checkout" "$dir/wt"
  bench "$dir/wt" "$mode-worktree" "$repo"
}

run_mode single
run_mode routed -routed
