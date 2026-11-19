// Package service is the relay's per-user service: the definitions (Launchd, Systemd,
// Windows) and the Manager that installs, removes and finds them.
package service

import (
	"html"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// StopWindow is how long the relay may take to stop before its service manager kills it.
// A token refresh the relay has sent runs to the end so its new pair reaches the store:
// api.RefreshBudget, 40 s, then the save, which is quick unless the keychain waits on a
// prompt. Killed sooner, the relay leaves the token stored marked as maybe spent, and the
// next refresh after the budget asks for a new sign-in instead of sending it. launchd's
// default is 20 s and systemd's may be lowered, so both definitions name it; on Windows
// the supervisor waits this long.
const StopWindow = time.Minute

// Launchd renders a relay agent that restarts only after an unsuccessful exit.
func Launchd(label, exe, logPath string, env map[string]string) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + esc(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + esc(exe) + `</string><string>relay</string><string>run</string><string>--service</string><string>--idle</string><string>0</string><string>--quiet</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
`)
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("    <key>" + esc(k) + "</key><string>" + esc(env[k]) + "</string>\n")
	}
	b.WriteString(`  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ExitTimeOut</key><integer>` + strconv.Itoa(int(StopWindow/time.Second)) + `</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + esc(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}
