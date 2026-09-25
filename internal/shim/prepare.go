package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Prepare writes data only: one file per prefix argument, followed by a versioned
// count. The launcher reads nothing until this process exits successfully. No shell
// code, credentials in stdout, or agent execution are part of this protocol.
func Prepare(agent, directory string, args []string) error {
	if !Routable(agent) {
		return fmt.Errorf("unsupported agent %q", agent)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	r := routeFor(agent, cwd, args)
	if len(r.args) > 256 {
		return fmt.Errorf("unsupported routing plan")
	}
	for i, arg := range r.args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("invalid argument")
		}
		if err := os.WriteFile(filepath.Join(directory, strconv.Itoa(i)), []byte(arg), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(directory, "count"), []byte(fmt.Sprintf("terma-args-v1:%d\n", len(r.args))), 0o600)
}

func shimScript(agent, binDir string) string {
	return shimHeader(agent) + " Managed by `terma install`.\n" +
		"terma_agent=" + shellQuote(agent) + "\nterma_bin=" + shellQuote(binDir) + "\n" + launcherBody
}

// shimHeader is how every version of the shim script has begun; it tells terma's
// script from anything else in the shim directory.
func shimHeader(agent string) string {
	return "#!/bin/sh\n# terma per-repo routing shim for " + agent + "."
}

const launcherBody = `# Resolve on every launch, including when Terma is absent or disabled.
terma_remaining=${PATH-}
terma_real=
while :; do
  case "$terma_remaining" in
    *:*) terma_entry=${terma_remaining%%:*}; terma_remaining=${terma_remaining#*:}; terma_more=1 ;;
    *) terma_entry=$terma_remaining; terma_more=0 ;;
  esac
  [ -n "$terma_entry" ] || terma_entry=.
  terma_candidate=$terma_entry/$terma_agent
  if [ -x "$terma_candidate" ] && [ ! -d "$terma_candidate" ] &&
     [ ! "$terma_entry" -ef "$terma_bin" ] && [ ! "$terma_candidate" -ef "$0" ]; then
    terma_real=$terma_candidate
    break
  fi
  [ "$terma_more" = 1 ] || break
 done
if [ -z "$terma_real" ]; then
  echo "$terma_agent is not installed (not on PATH)" >&2
  exit 127
fi
# These paths must not depend on a working Terma installation.
case "${TERMA_DISABLE-}" in 1|true) exec "$terma_real" "$@" ;; esac
case "${1-}" in
  --help|-h|help|--version|-V|version|update|upgrade|install|uninstall|login|logout|auth|completion|completions)
    exec "$terma_real" "$@" ;;
esac
command -v terma >/dev/null 2>&1 || exec "$terma_real" "$@"
terma_tmp=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/terma-route.XXXXXXXX") || exec "$terma_real" "$@"
terma_pid=
terma_timer=
terma_cleanup() {
  [ -z "$terma_timer" ] || kill "$terma_timer" 2>/dev/null
  [ -z "$terma_pid" ] || kill -9 "$terma_pid" 2>/dev/null
  /bin/rm -rf "$terma_tmp"
}
trap 'terma_cleanup' 0
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
# Redirect preparation's stdin too: it must never consume agent input.
terma shim prepare "$terma_agent" "$terma_tmp" -- "$@" </dev/null >/dev/null 2>&1 &
terma_pid=$!
(
  /bin/sleep 2
  kill -9 "$terma_pid" 2>/dev/null
) </dev/null >/dev/null 2>&1 &
terma_timer=$!
terma_ok=0
if wait "$terma_pid" 2>/dev/null; then terma_ok=1; fi
terma_pid=
kill "$terma_timer" 2>/dev/null
wait "$terma_timer" 2>/dev/null
terma_timer=
# Validate the complete data plan before changing any arguments.
terma_count=
if [ "$terma_ok" = 1 ] && [ -f "$terma_tmp/count" ]; then
  IFS= read -r terma_count < "$terma_tmp/count" || terma_ok=0
  case "$terma_count" in terma-args-v1:*) terma_count=${terma_count#terma-args-v1:} ;; *) terma_ok=0 ;; esac
  case "$terma_count" in ''|*[!0-9]*|????*|0[0-9]*) terma_ok=0 ;; esac
  if [ "$terma_ok" = 1 ]; then
    [ "$terma_count" -le 256 ] || terma_ok=0
    terma_i=0
    while [ "$terma_i" -lt "$terma_count" ]; do
      [ -f "$terma_tmp/$terma_i" ] && [ -r "$terma_tmp/$terma_i" ] || terma_ok=0
      terma_i=$((terma_i + 1))
    done
  fi
else
  terma_ok=0
fi
terma_launch_plan() {
  terma_i=$terma_count
  while [ "$terma_i" -gt 0 ]; do
    terma_i=$((terma_i - 1))
    # Sentinel preserves trailing newlines; never eval or source plan data.
    terma_arg=$(/bin/cat "$terma_tmp/$terma_i" && printf '.') || return 1
    terma_arg=${terma_arg%.}
    set -- "$terma_arg" "$@"
  done
  terma_cleanup
  trap - 0 INT TERM HUP
  if [ "$terma_agent" = codex ] && [ "$terma_count" -gt 0 ]; then
    TERMA_CODEX_ROUTED=1
    export TERMA_CODEX_ROUTED
  fi
  exec "$terma_real" "$@"
}
if [ "$terma_ok" = 1 ]; then
  terma_launch_plan "$@"
fi
echo "terma: routing preparation failed; starting $terma_agent without Terma overrides (TERMA_DISABLE=1 bypasses routing)." >&2
terma_cleanup
trap - 0 INT TERM HUP
exec "$terma_real" "$@"
`
