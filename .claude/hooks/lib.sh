# Shared by the PostToolUse hooks. Each runs in the edited file's own checkout, so a session
# editing a worktree fixes, formats, lints and checks that worktree.

# edited_files prints each existing file the tool call wrote, once (Edit/Write file_path, MultiEdit edits[]).
edited_files() {
  local input="$1" out=""
  if command -v jq >/dev/null 2>&1; then
    out="$(printf '%s' "$input" | jq -r '[.tool_input.file_path?, (.tool_input.edits? // [] | .[]?.file_path?)] | map(select(. != null and . != "")) | .[]' 2>/dev/null)"
  elif command -v python3 >/dev/null 2>&1; then
    out="$(printf '%s' "$input" | python3 -c '
import json, sys
ti = (json.load(sys.stdin).get("tool_input") or {})
for p in [ti.get("file_path")] + [(e or {}).get("file_path") for e in (ti.get("edits") or [])]:
    if p: print(p)
' 2>/dev/null)"
  fi
  printf '%s\n' "$out" | awk 'NF && !seen[$0]++' | while IFS= read -r p; do [ -f "$p" ] && printf '%s\n' "$p"; done
}

# go_package_dirs prints "<checkout root> <package dir>" once per edited Go package.
go_package_dirs() {
  printf '%s\n' "$1" | while IFS= read -r p; do
    case "$p" in
      *.go)
        d="$(dirname "$p")"
        root="$(git -C "$d" rev-parse --show-toplevel 2>/dev/null)" || continue
        printf '%s %s\n' "$root" "$d"
        ;;
    esac
  done | awk 'NF && !seen[$0]++'
}
