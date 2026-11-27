#!/usr/bin/env bash
# Says whether a capture job's run got to its end, for the publish job (capture-args.sh): writes
# <report>/capture.json, {"job", "harness", "finished"}, where finished is whether the run wrote
# its compat.json (TestMain does, once every test has run).
#
# capture-done.sh <report> <job: macos|linux> [harness]
set -euo pipefail

report=$1 job=$2 harness=${3:-}
mkdir -p "$report"
finished=false
if [ -f "$report/compat.json" ]; then finished=true; fi
jq -n --arg job "$job" --arg harness "$harness" --argjson finished "$finished" \
	'{job: $job, finished: $finished} + (if $harness == "" then {} else {harness: $harness} end)' >"$report/capture.json"
