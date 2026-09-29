package relay

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// launchdLabel names the macOS launch agent.
const launchdLabel = "ai.terma.relay"

// passedEnv is the environment a service manager gives the relay: the variables that
// decide where terma keeps its state and where Codex keeps its own, when the developer
// who ran setup had them set. A service starts with almost nothing, so without these the
// relay would read another config directory than the terma that configured it.
var passedEnv = []string{"TERMA_CONFIG_DIR", "XDG_CONFIG_HOME", "TERMA_ENV", "CODEX_HOME"}

func serviceEnv() [][2]string {
	var env [][2]string
	for _, k := range passedEnv {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env = append(env, [2]string{k, v})
		}
	}
	return env
}

// runCommand runs a service manager command; tests replace it.
var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func xmlUnescape(s string) string {
	var out string
	if err := xml.Unmarshal([]byte("<s>"+s+"</s>"), &out); err != nil {
		return s
	}
	return out
}

// launchdPlist renders the launch agent that keeps `<binary> relay serve` running.
func launchdPlist(binary string, env [][2]string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(binary) + `</string>
		<string>relay</string>
		<string>serve</string>
	</array>
`)
	if len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, kv := range env {
			b.WriteString("\t\t<key>" + xmlEscape(kv[0]) + "</key>\n\t\t<string>" + xmlEscape(kv[1]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>/dev/null</string>
	<key>StandardErrorPath</key>
	<string>/dev/null</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// systemdQuote quotes one ExecStart word or Environment assignment for a unit file.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`
}

func systemdUnquote(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`, `%%`, `%`, `$$`, `$`).Replace(s)
}

// systemdUnitFile renders the user unit that keeps `<binary> relay serve` running.
func systemdUnitFile(binary string, env [][2]string) []byte {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Terma telemetry relay\n\n[Service]\n")
	b.WriteString("ExecStart=" + systemdQuote(binary) + " relay serve\n")
	for _, kv := range env {
		b.WriteString("Environment=" + systemdQuote(kv[0]+"="+kv[1]) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// Where a written service definition names its binary, read back for ServiceState.
var (
	plistBinaryRe = regexp.MustCompile(`<key>ProgramArguments</key>\s*<array>\s*<string>([^<]*)</string>`)
	unitExecRe    = regexp.MustCompile(`(?m)^ExecStart="((?:[^"\\]|\\.)*)"`)
)
