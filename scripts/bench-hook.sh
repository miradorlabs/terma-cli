#!/bin/sh
# Measures the installed prepare-commit-msg shim end to end (sh + terma + chain)
# in a scratch repository and fails when it exceeds the budget. Runs the shim a
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
# --harness none: the benchmark times the git hook, and an install that selects an agent
# signs in for its key — on a developer's machine, where the agents are installed, that
# opened a browser login against production. The hooks below are driven by hand instead.
terma install --project proj_bench --harness none --yes >/dev/null

# An agent session with a manifest, so the timed path includes the staged-files
# intersection (the expensive branch), not the early exit.
printf '{"session_id":"018f3a2c-bench-session","hook_event_name":"SessionStart","cwd":"%s"}' "$repo" | terma hook session-start
mkdir -p src; echo "x" > src/a.go
printf '{"session_id":"018f3a2c-bench-session","hook_event_name":"PostToolUse","tool_name":"Edit","tool_input":{"file_path":"%s/src/a.go"},"cwd":"%s"}' "$repo" "$repo" | terma hook post-tool-use
git add src/a.go
echo "feat: bench" > msg.txt

times=""
i=0
while [ $i -lt 7 ]; do
  start=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')
  sh .terma/hooks/prepare-commit-msg msg.txt message
  end=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')
  times="$times $((end - start))"
  echo "feat: bench" > msg.txt
  i=$((i + 1))
done
median=$(echo "$times" | tr ' ' '\n' | grep -v '^$' | sort -n | sed -n 4p)
echo "prepare-commit-msg shim: median ${median}ms over 7 runs (${times# }) — budget ${BUDGET_MS}ms"
[ "$median" -le "$BUDGET_MS" ] || { echo "over budget" >&2; exit 1; }
