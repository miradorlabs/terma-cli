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
// A token refresh the relay has begun runs to the end so its new pair reaches the store.
// It holds the credential lock through its exchange, api.RefreshBudget (40 s), and at most
// two keychain writes, secret.WriteTimeout (a minute) each, which a keychain waiting on a
// prompt can take: the pair it holds, when the store missed it, and the pair it gets. So
// 160 s, and 20 s more for the credentials.json writes and the relay's own shutdown.
// Killed sooner, the relay may lose a pair the server has rotated to, and the token stored,
// marked as maybe spent, asks for a new sign-in once the budget is past. launchd's default
// is 20 s and systemd's may be lowered, so both definitions name it; on Windows the
// supervisor waits this long. TestRelayStopWindowCoversARefresh in internal/cli holds it
// to those bounds, which this package cannot import.
const StopWindow = 3 * time.Minute

// RestartWait is the longest Install waits for the relay it replaces to stop: StopWindow,
// and the moments the service manager takes to notice. Before that, StopRelay may wait up
// to 10 s for a hook-started relay to exit, ahead of the bootout. Whatever restarts the
// service must allow both: refreshTimeout in internal/cli is RestartWait and a minute,
// which covers StopRelay's 10 s.
const RestartWait = StopWindow + 5*time.Second

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
