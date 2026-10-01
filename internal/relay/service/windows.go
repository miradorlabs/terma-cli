package service

import (
	"maps"
	"slices"
	"strings"
)

// Windows is the script the Run key starts at logon: terma.exe started directly would open a
// console window at every logon, so wscript starts it hidden.
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
