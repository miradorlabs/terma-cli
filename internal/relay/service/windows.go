package service

import (
	"maps"
	"slices"
	"strings"
)

// Windows is the script the Run key starts at logon: wscript runs it with no
// window, and it starts `terma relay supervise` hidden (window style 0), not waiting —
// terma.exe is a console program, and started from the Run key directly it would open
// a console window at every logon. VBScript doubles a quote inside a string.
func Windows(exe string, env map[string]string) string {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var b strings.Builder
	b.WriteString("' terma's relay at logon: written by `terma relay daemon install`, removed by `terma relay daemon remove`.\r\n")
	b.WriteString("Set shell = CreateObject(\"WScript.Shell\")\r\n")
	b.WriteString("Set env = shell.Environment(\"Process\")\r\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		b.WriteString("env(" + q(k) + ") = " + q(env[k]) + "\r\n")
	}
	b.WriteString("shell.Run " + q(`"`+exe+`" relay supervise`) + ", 0, False\r\n")
	return b.String()
}
