#!/bin/sh
# Measures the prepare-commit-msg shim git's global hooks path runs, end to end (sh +
# terma), in a scratch repository the team policy lists, and fails when it exceeds the
# budget. Runs the shim a
# few times and takes the median: the first execution of a freshly written script
# pays a one-time OS cost (macOS's exec policy check) that no commit after it sees.
set -eu
BUDGET_MS="${TERMA_HOOK_BUDGET_MS:-50}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin/terma"
[ -x "$BIN" ] || { echo "build first: make build" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export TERMA_CONFIG_DIR="$tmp/home" PATH="$ROOT/bin:$PATH"
repo="$tmp/repo"; mkdir -p "$repo"; cd "$repo"
git init -q; git config user.email bench@example.com; git config user.name bench
# What `terma setup` leaves, offline: a validated policy listing this folder, and the
# shim it writes into git's global hooks directory.
export TERMA_POLICY_STUB='{"mode":"repo","folders":["repo"]}'
mkdir -p "$TERMA_CONFIG_DIR" hooks
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '{"active_profile":"default","profiles":{"default":{"policy":{"mode":"repo","folders":["repo"],"include_prompts":true,"include_tool_content":true,"revision":1,"updated_at":"%s","team_id":"proj_bench","fetched_at":"%s"}}}}' "$now" "$now" > "$TERMA_CONFIG_DIR/config.json"
printf '#!/bin/sh\n[ -x %s ] && %s hook prepare-commit-msg "$@" || true\nexit 0\n' "$BIN" "$BIN" > hooks/prepare-commit-msg

# An agent session with a manifest, so the timed path includes the staged-files
# intersection (the expensive branch), not the early exit.
printf '{"session_id":"018f3a2c-bench-session","hook_event_name":"SessionStart","cwd":"%s"}' "$repo" | terma hook session-start
mkdir -p src; echo "x" > src/a.go
printf '{"session_id":"018f3a2c-bench-session","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"%s/src/a.go"},"cwd":"%s"}' "$repo" "$repo" | terma hook post-tool-use
git add src/a.go
echo "feat: bench" > msg.txt
sh hooks/prepare-commit-msg msg.txt message
grep -q 'Agent-Session-Id' msg.txt || { echo "the shim did not stamp: the timed path is not the real one" >&2; exit 1; }
echo "feat: bench" > msg.txt

times=""
i=0
while [ $i -lt 7 ]; do
  start=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')
  sh hooks/prepare-commit-msg msg.txt message
  end=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')
  times="$times $((end - start))"
  echo "feat: bench" > msg.txt
  i=$((i + 1))
done
median=$(echo "$times" | tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p)
echo "prepare-commit-msg shim: median ${median}ms over 7 runs (${times# }) — budget ${BUDGET_MS}ms"
[ "$median" -le "$BUDGET_MS" ] || { echo "over budget" >&2; exit 1; }
