package relay

import (
	"regexp"
	"strings"
)

// windowsLauncher renders the wscript file the Run key starts at logon: it sets the
// environment passedEnv carries and runs `<binary> relay supervise` with its window
// hidden (style 0), without waiting. VBScript escapes a quote by doubling it.
func windowsLauncher(binary string, env [][2]string) []byte {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var b strings.Builder
	b.WriteString("' terma relay launcher, written by `terma setup`.\r\n")
	b.WriteString("Set sh = CreateObject(\"WScript.Shell\")\r\n")
	b.WriteString("Set env = sh.Environment(\"PROCESS\")\r\n")
	for _, kv := range env {
		b.WriteString("env(" + q(kv[0]) + ") = " + q(kv[1]) + "\r\n")
	}
	b.WriteString("sh.Run " + q(`"`+binary+`" relay supervise`) + ", 0, False\r\n")
	return []byte(b.String())
}

// launcherBinaryRe finds the binary in the launcher's Run line. A Windows path cannot
// contain a quote, so the doubled quotes around it are its only ones.
var launcherBinaryRe = regexp.MustCompile(`(?m)^sh\.Run """([^"]+)"" relay supervise"`)

// launcherBinary reads back the binary a launcher runs.
func launcherBinary(data []byte) string {
	m := launcherBinaryRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}
