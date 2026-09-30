// Package service renders per-user relay service definitions. Service-manager
// execution remains with the CLI, which owns lifecycle and reporting.
package service

import (
	"html"
	"maps"
	"slices"
	"strings"
)

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
    <string>` + esc(exe) + `</string><string>relay</string><string>run</string><string>--idle</string><string>0</string><string>--quiet</string>
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
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>` + esc(logPath) + `</string>
  <key>StandardErrorPath</key><string>` + esc(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}
